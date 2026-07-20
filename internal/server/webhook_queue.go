package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// WebhookBackend is implemented by backends that can persist webhook
// subscribers and delivery state. sqliteBackend implements it; memBackend
// does not — the in-memory slice on Store handles everything for tests and
// local dev.
type WebhookBackend interface {
	SaveWebhook(w Webhook) error
	LoadWebhooks() ([]Webhook, error)
	RemoveWebhook(id string) error
	EnqueueDelivery(d PendingDelivery) error
	FetchDueDeliveries(now time.Time, limit int) ([]PendingDelivery, error)
	UpdateDelivery(id string, attempt int, nextAt time.Time, status int, errMsg string) error
	DeleteDelivery(id string) error
	CancelDeliveriesForWebhook(subscriberID string) error
}

// PendingDelivery is a unit of work in the retry queue.
// Subscriber is populated by FetchDueDeliveries via JOIN with webhook_subscribers.
type PendingDelivery struct {
	ID         string
	Subscriber Webhook
	KnockJSON  string
	Attempt    int // number of completed attempts (0 = never tried)
	NextAt     time.Time
	LastStatus int
	LastError  string
}

// DeliveryQueue is a DB-backed retry queue. It polls for due deliveries on a
// fixed interval and dispatches them using the Dispatcher.
// Only instantiated when the backend implements WebhookBackend (SQLite mode).
//
// On each tick: fetch rows where next_at_ns <= now, dispatch each in a
// goroutine. In-flight tracking (inFlight map) prevents re-dispatching a
// delivery that a concurrent goroutine is already handling.
type DeliveryQueue struct {
	backend      WebhookBackend
	dispatcher   *Dispatcher
	pollInterval time.Duration

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	inFlight map[string]bool
}

// NewDeliveryQueue constructs a queue. Call Start() to begin polling.
func NewDeliveryQueue(backend WebhookBackend, dispatcher *Dispatcher) *DeliveryQueue {
	return &DeliveryQueue{
		backend:      backend,
		dispatcher:   dispatcher,
		pollInterval: 2 * time.Second,
		inFlight:     make(map[string]bool),
	}
}

// Start begins the background polling goroutine. It immediately processes any
// deliveries that were pending before the last restart.
func (q *DeliveryQueue) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	q.wg.Add(1)
	go q.loop(ctx)
}

// Stop signals the poll loop to exit and waits for all in-flight dispatches
// to complete. Must be called before closing the underlying backend.
func (q *DeliveryQueue) Stop() {
	if q.cancel != nil {
		q.cancel()
	}
	q.wg.Wait()
}

// Enqueue persists a delivery for sub/k to the DB so the poll loop will pick
// it up. Called by fanOutWebhooks in SQLite mode.
func (q *DeliveryQueue) Enqueue(sub Webhook, k Knock) error {
	body, err := json.Marshal(k)
	if err != nil {
		return fmt.Errorf("enqueue marshal: %w", err)
	}
	return q.backend.EnqueueDelivery(PendingDelivery{
		ID:         NewID(),
		Subscriber: sub,
		KnockJSON:  string(body),
		Attempt:    0,
		NextAt:     time.Now(),
	})
}

func (q *DeliveryQueue) loop(ctx context.Context) {
	defer q.wg.Done()
	t := time.NewTicker(q.pollInterval)
	defer t.Stop()
	// Run immediately on startup to resume deliveries from before the restart.
	q.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.tick()
		}
	}
}

func (q *DeliveryQueue) tick() {
	deliveries, err := q.backend.FetchDueDeliveries(time.Now(), 50)
	if err != nil || len(deliveries) == 0 {
		return
	}
	for _, d := range deliveries {
		d := d
		q.mu.Lock()
		if q.inFlight[d.ID] {
			q.mu.Unlock()
			continue
		}
		q.inFlight[d.ID] = true
		q.mu.Unlock()

		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			defer func() {
				q.mu.Lock()
				delete(q.inFlight, d.ID)
				q.mu.Unlock()
			}()
			q.dispatch(d)
		}()
	}
}

// dispatch performs a single delivery attempt and updates the DB record based
// on the outcome. Mirrors the retry/backoff semantics of Dispatcher.Deliver:
// - 2xx → success, delete record
// - 4xx (except 429) → permanent failure, delete record
// - transport errors, 5xx, 429 → schedule next attempt with exponential backoff
// - after MaxAttempts total attempts → give up, delete record
func (q *DeliveryQueue) dispatch(d PendingDelivery) {
	rawBody := []byte(d.KnockJSON)

	var k Knock
	if err := json.Unmarshal(rawBody, &k); err != nil {
		// Corrupt record — drop it.
		_ = q.backend.DeleteDelivery(d.ID)
		return
	}

	sig := SignBody(rawBody, d.Subscriber.Secret)
	currentAttempt := d.Attempt + 1

	req, err := http.NewRequest(http.MethodPost, d.Subscriber.URL, bytes.NewReader(rawBody))
	if err != nil {
		// Permanently bad URL.
		_ = q.backend.DeleteDelivery(d.ID)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", q.dispatcher.UserAgent)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Dingdong-Webhook-Id", d.Subscriber.ID)
	req.Header.Set("X-Dingdong-Knock-Id", k.ID)
	req.Header.Set("X-Dingdong-Topic", k.Topic)
	req.Header.Set("X-Dingdong-Delivery-Attempt", strconv.Itoa(currentAttempt))

	resp, err := q.dispatcher.Client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			_ = q.backend.DeleteDelivery(d.ID)
			return
		}
		// 4xx except 429: subscriber config bug, not worth retrying.
		if resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode >= 400 && resp.StatusCode < 500 {
			_ = q.backend.DeleteDelivery(d.ID)
			return
		}
		// Retryable (5xx or 429).
		if currentAttempt >= q.dispatcher.MaxAttempts {
			_ = q.backend.DeleteDelivery(d.ID)
			return
		}
		backoff := time.Duration(1<<(currentAttempt-1)) * time.Second
		_ = q.backend.UpdateDelivery(d.ID, currentAttempt, time.Now().Add(backoff), resp.StatusCode, "")
		return
	}

	// Transport error.
	if currentAttempt >= q.dispatcher.MaxAttempts {
		_ = q.backend.DeleteDelivery(d.ID)
		return
	}
	backoff := time.Duration(1<<(currentAttempt-1)) * time.Second
	_ = q.backend.UpdateDelivery(d.ID, currentAttempt, time.Now().Add(backoff), 0, err.Error())
}
