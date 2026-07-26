package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Webhook is a subscriber that receives POSTs of matching knocks. The Secret
// is returned ONLY in the response to POST /v1/webhooks; subsequent listings
// omit it. There's no way to retrieve it after creation — re-register if
// you lose it.
//
// Topic narrows the subscription: empty means "all topics" (wildcard).
type Webhook struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Topic     string    `json:"topic,omitempty"`
	Secret    string    `json:"secret,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AddWebhook stores w with a fresh ID + CreatedAt. If w.Secret is empty,
// a random 32-byte hex secret is generated so the caller always gets a
// signing secret back in the response.
func (s *Store) AddWebhook(w Webhook) Webhook {
	if w.Secret == "" {
		w.Secret = randomSecret()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.ID = NewID()
	w.CreatedAt = time.Now().UTC()
	s.webhooks = append(s.webhooks, w)
	if s.webhookDB != nil {
		_ = s.webhookDB.SaveWebhook(w)
	}
	return w
}

// ListWebhooks returns a copy of every registered webhook with Secret
// scrubbed. Safe to expose over the public API.
func (s *Store) ListWebhooks() []Webhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Webhook, len(s.webhooks))
	for i, w := range s.webhooks {
		w.Secret = ""
		out[i] = w
	}
	return out
}

// DeleteWebhook removes the webhook by id. Returns true if found.
// When using the SQLite backend, also cancels any pending deliveries for this
// subscriber and removes the subscriber record from the DB.
func (s *Store) DeleteWebhook(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, w := range s.webhooks {
		if w.ID == id {
			s.webhooks = append(s.webhooks[:i], s.webhooks[i+1:]...)
			if s.webhookDB != nil {
				_ = s.webhookDB.CancelDeliveriesForWebhook(id)
				_ = s.webhookDB.RemoveWebhook(id)
			}
			return true
		}
	}
	return false
}

// MatchingWebhooks returns subscribers whose Topic is empty (wildcard) or
// equal to topic. The returned slice carries the Secret — internal use
// only, for the dispatcher.
func (s *Store) MatchingWebhooks(topic string) []Webhook {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Webhook, 0)
	for _, w := range s.webhooks {
		if w.Topic == "" || w.Topic == topic {
			out = append(out, w)
		}
	}
	return out
}

// randomSecret returns a 64-char hex string (256 bits of entropy).
func randomSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure means the OS is on fire. Fall back to a
		// time-derived secret rather than panicking; callers can always
		// re-register.
		return hex.EncodeToString([]byte(fmt.Sprintf("fallback-%d", time.Now().UnixNano())))
	}
	return hex.EncodeToString(b[:])
}

// Dispatcher delivers knocks to webhook subscribers with bounded
// exponential-backoff retries. Fields are read-only after construction;
// replace the whole Dispatcher (before any deliveries are in flight) to
// reconfigure.
type Dispatcher struct {
	Client      *http.Client
	Sleep       func(time.Duration)
	MaxAttempts int
	UserAgent   string
}

// NewDispatcher returns a Dispatcher with sane defaults for production.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		Client:      &http.Client{Timeout: 10 * time.Second},
		Sleep:       time.Sleep,
		MaxAttempts: 4,
		UserAgent:   "dingdong-webhook/1",
	}
}

// Deliver POSTs k to w with retry/backoff. Synchronous — callers are
// expected to run on their own goroutine.
//
// Behavior:
//   - Always signs the body with HMAC-SHA256 over the raw JSON using
//     w.Secret, sending `X-Hub-Signature-256: sha256=<hex>` (the GitHub /
//     Hermes-compatible shape).
//   - Retries on transport errors, 5xx, and 429.
//   - Stops on 2xx (success) or 4xx-except-429 (subscriber config bug).
//   - Backoff starts at 1s and doubles, capped by MaxAttempts.
//
// MVP caveat: in-memory only. A pod restart drops all pending retries.
func (d *Dispatcher) Deliver(w Webhook, k Knock) {
	body, err := json.Marshal(k)
	if err != nil {
		return
	}
	sig := SignBody(body, w.Secret)

	backoff := time.Second
	for attempt := 1; attempt <= d.MaxAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			// Bad URL — won't recover, abandon.
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", d.UserAgent)
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-Dingdong-Webhook-Id", w.ID)
		req.Header.Set("X-Dingdong-Knock-Id", k.ID)
		req.Header.Set("X-Dingdong-Topic", k.Topic)
		req.Header.Set("X-Dingdong-Delivery-Attempt", fmt.Sprintf("%d", attempt))

		resp, err := d.Client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
			// 4xx (except 429) are client-config bugs — retrying won't help.
			if resp.StatusCode != http.StatusTooManyRequests &&
				resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return
			}
		}
		if attempt == d.MaxAttempts {
			return
		}
		d.Sleep(backoff)
		backoff *= 2
	}
}

// SignBody returns `sha256=<hex>` for the HMAC-SHA256 of body keyed by
// secret. Matches the GitHub / Hermes webhook validator shape.
func SignBody(body []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// validateWebhookURL ensures the URL is well-formed and uses an HTTP scheme.
// Anything else (file://, ftp://, gopher://) is a config bug and we'd
// rather reject at registration than fail per-delivery.
func validateWebhookURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url is invalid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url scheme must be http or https")
	}
	if u.Host == "" {
		return errors.New("url must have a host")
	}
	return nil
}

const maxWebhookBytes = 8 << 10 // 8 KiB — registrations are tiny

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBytes)
	var in Webhook
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateWebhookURL(in.URL); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stored := s.store.AddWebhook(Webhook{
		URL:    in.URL,
		Topic:  in.Topic,
		Secret: in.Secret,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(stored)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.store.ListWebhooks())
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if !s.store.DeleteWebhook(id) {
		http.Error(w, "webhook not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fanOutWebhooks finds matching subscribers and dispatches the knock to each.
// In SQLite mode (deliveryQueue non-nil), deliveries are persisted to the DB
// and dispatched by the background poll loop — surviving pod restarts.
// In mem mode (tests/local dev), each delivery runs on its own goroutine and
// is tracked by deliveryWG so WaitForDeliveries works.
func (s *Server) fanOutWebhooks(k Knock) {
	subs := s.store.MatchingWebhooks(k.Topic)
	for _, sub := range subs {
		sub := sub
		if s.deliveryQueue != nil {
			_ = s.deliveryQueue.Enqueue(sub, k)
		} else {
			s.deliveryWG.Add(1)
			go func(target Webhook) {
				defer s.deliveryWG.Done()
				s.dispatcher.Deliver(target, k)
			}(sub)
		}
	}
}

// WaitForDeliveries blocks until every webhook delivery launched so far has
// returned (success, give-up, or context cancel). Intended for tests and
// graceful shutdown; production callers should not block on it.
func (s *Server) WaitForDeliveries() {
	s.deliveryWG.Wait()
}
