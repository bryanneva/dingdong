package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newTestSQLiteServer opens a SQLite-backed Server at the given db path and
// returns the httptest.Server and the *Server. Callers are responsible for
// closing both (ts.Close then srv.Close, in that order).
func newTestSQLiteServer(t *testing.T, dbPath string) (*httptest.Server, *Server) {
	t.Helper()
	store, err := NewSQLiteStore(dbPath, 1000)
	if err != nil {
		t.Fatalf("new sqlite store: %v", err)
	}
	srv := New(Config{Token: testToken, Store: store})
	ts := httptest.NewServer(srv)
	return ts, srv
}

// TestWebhookSubscriberPersistence_SurvivesReopen is the regression-detector
// for issue #72 exit criterion 1: a registered subscriber must still be
// present after close + reopen of the SQLite backend.
//
// Pre-change failure: sqliteBackend had no webhook_subscribers table and no
// SaveWebhook/LoadWebhooks methods. This test would fail to compile.
func TestWebhookSubscriberPersistence_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Session 1: register via the HTTP API.
	ts1, srv1 := newTestSQLiteServer(t, dbPath)
	sub := registerWebhook(t, ts1, `{"url":"http://example.com/hook","topic":"ops","secret":"shh"}`)
	ts1.Close()
	if err := srv1.Close(); err != nil {
		t.Fatalf("close srv1: %v", err)
	}

	// Session 2: reopen same DB and list via HTTP.
	ts2, srv2 := newTestSQLiteServer(t, dbPath)
	defer ts2.Close()
	defer srv2.Close()

	resp := doRequest(t, bearerReq(t, http.MethodGet, ts2.URL+"/v1/webhooks", ""))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d, want 200", resp.StatusCode)
	}
	var got []Webhook
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("after restart: len=%d, want 1", len(got))
	}
	if got[0].ID != sub.ID {
		t.Errorf("after restart: id=%q, want %q", got[0].ID, sub.ID)
	}
	if got[0].URL != "http://example.com/hook" {
		t.Errorf("after restart: url=%q, want http://example.com/hook", got[0].URL)
	}
	if got[0].Topic != "ops" {
		t.Errorf("after restart: topic=%q, want ops", got[0].Topic)
	}
	// Secret must NOT appear in list response (redacted on GET, even from DB).
	if got[0].Secret != "" {
		t.Errorf("after restart: secret leaked in list response: %q", got[0].Secret)
	}
}

// TestWebhookDeliveryPersistence_QueueSurvivesReopen is the regression-detector
// for exit criterion 2: failed delivery state must survive restart.
// It works directly against sqliteBackend to avoid the HTTP server's timing
// complexities (a real restart test would need to drive the delivery loop to
// failure mid-backoff, which is brittle in a unit test).
//
// Pre-change failure: sqliteBackend had no EnqueueDelivery/FetchDueDeliveries.
func TestWebhookDeliveryPersistence_QueueSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	b1, err := newSQLiteBackend(dbPath, 1000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	sub := Webhook{
		ID:        "wh001",
		URL:       "http://example.com/hook",
		Secret:    "shh",
		CreatedAt: time.Now().UTC(),
	}
	if err := b1.SaveWebhook(sub); err != nil {
		t.Fatalf("save webhook: %v", err)
	}

	// Simulate: delivery attempt 2 failed, next retry due at past time.
	d := PendingDelivery{
		ID:         "del001",
		Subscriber: sub,
		KnockJSON:  `{"id":"kn001","topic":"ops","from":"a","kind":"info","ts":"2024-01-01T00:00:00Z"}`,
		Attempt:    2,
		NextAt:     time.Now().Add(-1 * time.Second),
	}
	if err := b1.EnqueueDelivery(d); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen — simulates pod restart.
	b2, err := newSQLiteBackend(dbPath, 1000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b2.Close()

	deliveries, err := b2.FetchDueDeliveries(time.Now(), 10)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("after restart: len=%d, want 1 (pending delivery must survive reopen)", len(deliveries))
	}
	if deliveries[0].ID != "del001" {
		t.Errorf("id=%q, want del001", deliveries[0].ID)
	}
	if deliveries[0].Attempt != 2 {
		t.Errorf("attempt=%d, want 2 (retry count must survive reopen)", deliveries[0].Attempt)
	}
	if deliveries[0].Subscriber.URL != sub.URL {
		t.Errorf("subscriber url=%q, want %q", deliveries[0].Subscriber.URL, sub.URL)
	}
	if deliveries[0].Subscriber.Secret != sub.Secret {
		t.Errorf("subscriber secret not loaded (needed for HMAC signing after restart)")
	}
}

// TestWebhookDeletion_CancelsQueuedRetries is the regression-detector for
// exit criterion 3: deleting a subscriber must remove its queued retries so
// no zombie dispatches occur after restart.
//
// Pre-change failure: sqliteBackend had no CancelDeliveriesForWebhook.
func TestWebhookDeletion_CancelsQueuedRetries(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	b, err := newSQLiteBackend(dbPath, 1000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	sub := Webhook{
		ID:        "wh001",
		URL:       "http://example.com/hook",
		Secret:    "shh",
		CreatedAt: time.Now().UTC(),
	}
	if err := b.SaveWebhook(sub); err != nil {
		t.Fatalf("save webhook: %v", err)
	}
	d := PendingDelivery{
		ID:         "del001",
		Subscriber: sub,
		KnockJSON:  `{"id":"kn001","topic":"ops","from":"a","kind":"info","ts":"2024-01-01T00:00:00Z"}`,
		Attempt:    1,
		NextAt:     time.Now().Add(-1 * time.Second),
	}
	if err := b.EnqueueDelivery(d); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Delete the subscriber — must cascade to deliveries.
	if err := b.CancelDeliveriesForWebhook(sub.ID); err != nil {
		t.Fatalf("cancel deliveries: %v", err)
	}
	if err := b.RemoveWebhook(sub.ID); err != nil {
		t.Fatalf("remove webhook: %v", err)
	}

	// No deliveries should be due.
	deliveries, err := b.FetchDueDeliveries(time.Now(), 10)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(deliveries) != 0 {
		t.Errorf("after delete: len=%d, want 0 (subscriber deletion must cancel queued retries)", len(deliveries))
	}

	// No subscribers should be listed.
	webhooks, err := b.LoadWebhooks()
	if err != nil {
		t.Fatalf("load webhooks: %v", err)
	}
	if len(webhooks) != 0 {
		t.Errorf("after delete: %d webhooks, want 0", len(webhooks))
	}
}

// TestWebhookDeletion_ViaHTTP_CancelsQueuedRetries exercises the full
// DELETE /v1/webhooks/{id} path with a SQLite-backed server. This ensures
// the Store.DeleteWebhook method correctly calls CancelDeliveriesForWebhook
// and RemoveWebhook on the SQLite backend when a subscriber is removed.
func TestWebhookDeletion_ViaHTTP_CancelsQueuedRetries(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	ts, srv := newTestSQLiteServer(t, dbPath)
	defer ts.Close()
	defer srv.Close()

	sub := registerWebhook(t, ts, `{"url":"http://example.com/hook"}`)

	// Manually enqueue a delivery so we have something to cancel.
	d := PendingDelivery{
		ID:         "del001",
		Subscriber: Webhook{ID: sub.ID, URL: sub.URL, Secret: sub.Secret},
		KnockJSON:  `{"id":"kn001","topic":"ops","from":"a","kind":"info","ts":"2024-01-01T00:00:00Z"}`,
		Attempt:    1,
		NextAt:     time.Now().Add(10 * time.Minute), // far future so delivery loop won't touch it
	}
	if err := srv.store.webhookDB.EnqueueDelivery(d); err != nil {
		t.Fatalf("enqueue delivery: %v", err)
	}

	// Delete the subscriber via HTTP.
	resp := doRequest(t, bearerReq(t, http.MethodDelete, ts.URL+"/v1/webhooks/"+sub.ID, ""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d, want 204", resp.StatusCode)
	}

	// Verify delivery was cancelled in the DB.
	deliveries, err := srv.store.webhookDB.FetchDueDeliveries(time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(deliveries) != 0 {
		t.Errorf("after HTTP delete: %d deliveries remain, want 0", len(deliveries))
	}
}
