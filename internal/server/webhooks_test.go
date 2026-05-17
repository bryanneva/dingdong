package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFastDispatcher swaps the server's dispatcher for one that doesn't
// sleep between retries. Must be called BEFORE any /v1/knocks is posted —
// the dispatcher must be installed before any goroutine could observe the
// old one.
func withFastDispatcher(srv *Server, maxAttempts int) {
	srv.dispatcher = &Dispatcher{
		Client:      http.DefaultClient,
		Sleep:       func(time.Duration) {},
		MaxAttempts: maxAttempts,
		UserAgent:   "dingdong-test/1",
	}
}

func decodeWebhook(t *testing.T, body io.Reader) Webhook {
	t.Helper()
	var w Webhook
	if err := json.NewDecoder(body).Decode(&w); err != nil {
		t.Fatalf("decode webhook: %v", err)
	}
	return w
}

func registerWebhook(t *testing.T, ts *httptest.Server, body string) Webhook {
	t.Helper()
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/webhooks", body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status=%d body=%s", resp.StatusCode, buf)
	}
	return decodeWebhook(t, resp.Body)
}

func TestSignBody_KnownVector(t *testing.T) {
	// Canonical example from GitHub's webhook signing docs.
	//   secret = "It's a Secret to Everybody"
	//   body   = "Hello, World!"
	got := SignBody([]byte("Hello, World!"), "It's a Secret to Everybody")
	want := "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got != want {
		t.Errorf("SignBody mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestValidateWebhookURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"http", "http://example.com/hook", true},
		{"https", "https://example.com/hook", true},
		{"https with path", "https://example.com:8443/api/v1/hook?x=1", true},
		{"empty", "", false},
		{"ftp scheme", "ftp://example.com", false},
		{"file scheme", "file:///etc/passwd", false},
		{"no host", "http:///path", false},
		{"parse error", "http://[::1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebhookURL(tt.in)
			if tt.ok && err != nil {
				t.Errorf("validateWebhookURL(%q) = %v, want nil", tt.in, err)
			}
			if !tt.ok && err == nil {
				t.Errorf("validateWebhookURL(%q) = nil, want error", tt.in)
			}
		})
	}
}

func TestCreateWebhook_HappyPath(t *testing.T) {
	ts, srv := newTestServer(t)
	body := `{"url":"http://example.com/hook","topic":"ops"}`
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/webhooks", body))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s, want 201", resp.StatusCode, buf)
	}
	got := decodeWebhook(t, resp.Body)
	if got.URL != "http://example.com/hook" {
		t.Errorf("URL=%q, want http://example.com/hook", got.URL)
	}
	if got.Topic != "ops" {
		t.Errorf("Topic=%q, want ops", got.Topic)
	}
	if got.Secret == "" {
		t.Error("Secret should be auto-generated when not provided")
	}
	if got.ID == "" {
		t.Error("ID should be assigned")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be assigned")
	}

	// Store has the registration
	stored := srv.store.ListWebhooks()
	if len(stored) != 1 || stored[0].ID != got.ID {
		t.Errorf("store=%v, want one webhook with id %s", stored, got.ID)
	}
}

func TestCreateWebhook_UserSuppliedSecret(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/webhooks", `{"url":"http://example.com/h","secret":"my-secret"}`))
	defer resp.Body.Close()
	got := decodeWebhook(t, resp.Body)
	if got.Secret != "my-secret" {
		t.Errorf("Secret=%q, want my-secret (caller-supplied secret must round-trip)", got.Secret)
	}
}

func TestCreateWebhook_WildcardTopic(t *testing.T) {
	ts, _ := newTestServer(t)
	got := registerWebhook(t, ts, `{"url":"http://example.com/h"}`)
	if got.Topic != "" {
		t.Errorf("Topic=%q, want empty (wildcard)", got.Topic)
	}
}

func TestCreateWebhook_InvalidInput(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing url", `{"topic":"ops"}`, http.StatusBadRequest},
		{"empty url", `{"url":""}`, http.StatusBadRequest},
		{"non-http scheme", `{"url":"ftp://example.com"}`, http.StatusBadRequest},
		{"bad json", `{not-json}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/webhooks", tc.body))
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				buf, _ := io.ReadAll(resp.Body)
				t.Errorf("status=%d body=%s, want %d", resp.StatusCode, buf, tc.want)
			}
		})
	}
}

func TestCreateWebhook_OversizedBody(t *testing.T) {
	ts, _ := newTestServer(t)
	big := strings.Repeat("x", maxWebhookBytes+1024)
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/webhooks",
		`{"url":"http://example.com/h","topic":"`+big+`"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413", resp.StatusCode)
	}
}

func TestListWebhooks_ExcludesSecret(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.AddWebhook(Webhook{URL: "http://a.example/h", Topic: "ops", Secret: "shh"})
	srv.store.AddWebhook(Webhook{URL: "http://b.example/h", Secret: "shh2"})

	resp := doRequest(t, bearerReq(t, http.MethodGet, ts.URL+"/v1/webhooks", ""))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got []Webhook
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	for _, w := range got {
		if w.Secret != "" {
			t.Errorf("Secret leaked in list response: %q (url=%s)", w.Secret, w.URL)
		}
		if w.URL == "" {
			t.Error("URL should be set in list response")
		}
	}
}

func TestListWebhooks_Empty(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodGet, ts.URL+"/v1/webhooks", ""))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	var got []Webhook
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d, want 0", len(got))
	}
}

func TestDeleteWebhook(t *testing.T) {
	ts, srv := newTestServer(t)
	w := srv.store.AddWebhook(Webhook{URL: "http://a.example/h"})

	resp := doRequest(t, bearerReq(t, http.MethodDelete, ts.URL+"/v1/webhooks/"+w.ID, ""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status=%d, want 204", resp.StatusCode)
	}
	if got := srv.store.ListWebhooks(); len(got) != 0 {
		t.Errorf("after delete: len=%d, want 0", len(got))
	}
}

func TestDeleteWebhook_NotFound(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodDelete, ts.URL+"/v1/webhooks/nonexistent", ""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}

func TestWebhookEndpoints_RequireAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := []struct {
		method, path string
	}{
		{"POST", "/v1/webhooks"},
		{"GET", "/v1/webhooks"},
		{"DELETE", "/v1/webhooks/anything"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(""))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status=%d, want 401 (no Authorization header)", resp.StatusCode)
			}
		})
	}
}

// receiver wraps an httptest.Server that records every webhook delivery.
type receiver struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  []receivedReq
	respondFn func(attempt int32, w http.ResponseWriter, r *http.Request)
	attempts  int32
}

type receivedReq struct {
	body      []byte
	headers   http.Header
	signature string
	method    string
	path      string
}

func newReceiver() *receiver {
	rec := &receiver{}
	rec.respondFn = func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&rec.attempts, 1)
		body, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, receivedReq{
			body:      body,
			headers:   r.Header.Clone(),
			signature: r.Header.Get("X-Hub-Signature-256"),
			method:    r.Method,
			path:      r.URL.Path,
		})
		rec.mu.Unlock()
		rec.respondFn(n, w, r)
	}))
	return rec
}

func (r *receiver) URL() string         { return r.srv.URL }
func (r *receiver) Close()              { r.srv.Close() }
func (r *receiver) AttemptCount() int32 { return atomic.LoadInt32(&r.attempts) }
func (r *receiver) RespondWith(fn func(attempt int32, w http.ResponseWriter, req *http.Request)) {
	r.respondFn = fn
}

func (r *receiver) Received() []receivedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]receivedReq, len(r.requests))
	copy(out, r.requests)
	return out
}

func postKnock(t *testing.T, ts *httptest.Server, jsonBody string) Knock {
	t.Helper()
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", jsonBody))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("post knock: status=%d body=%s", resp.StatusCode, buf)
	}
	var k Knock
	if err := json.NewDecoder(resp.Body).Decode(&k); err != nil {
		t.Fatalf("decode knock: %v", err)
	}
	return k
}

func TestWebhookDelivery_HappyPath(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	sub := registerWebhook(t, ts, `{"url":"`+rec.URL()+`","topic":"ops","secret":"shh"}`)
	knock := postKnock(t, ts, `{"from":"alice","topic":"ops","subject":"hi"}`)
	srv.WaitForDeliveries()

	got := rec.Received()
	if len(got) != 1 {
		t.Fatalf("len(received)=%d, want 1", len(got))
	}
	r := got[0]

	if r.method != http.MethodPost {
		t.Errorf("method=%q, want POST", r.method)
	}
	wantSig := SignBody(r.body, "shh")
	if r.signature != wantSig {
		t.Errorf("signature=%q, want %q", r.signature, wantSig)
	}
	if r.headers.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", r.headers.Get("Content-Type"))
	}
	if r.headers.Get("X-Dingdong-Webhook-Id") != sub.ID {
		t.Errorf("X-Dingdong-Webhook-Id=%q, want %q", r.headers.Get("X-Dingdong-Webhook-Id"), sub.ID)
	}
	if r.headers.Get("X-Dingdong-Knock-Id") != knock.ID {
		t.Errorf("X-Dingdong-Knock-Id=%q, want %q", r.headers.Get("X-Dingdong-Knock-Id"), knock.ID)
	}
	if r.headers.Get("X-Dingdong-Topic") != "ops" {
		t.Errorf("X-Dingdong-Topic=%q, want ops", r.headers.Get("X-Dingdong-Topic"))
	}
	if r.headers.Get("X-Dingdong-Delivery-Attempt") != "1" {
		t.Errorf("X-Dingdong-Delivery-Attempt=%q, want 1", r.headers.Get("X-Dingdong-Delivery-Attempt"))
	}

	var sent Knock
	if err := json.Unmarshal(r.body, &sent); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, r.body)
	}
	if sent.From != "alice" || sent.Subject != "hi" || sent.Topic != "ops" {
		t.Errorf("decoded knock = %+v", sent)
	}
}

func TestWebhookDelivery_WildcardReceivesAllTopics(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	registerWebhook(t, ts, `{"url":"`+rec.URL()+`"}`)
	for _, topic := range []string{"ops", "marketing", "agents"} {
		postKnock(t, ts, `{"from":"a","topic":"`+topic+`"}`)
	}
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 3 {
		t.Errorf("attempts=%d, want 3 (wildcard receives all topics)", got)
	}
}

func TestWebhookDelivery_TopicFilter(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	registerWebhook(t, ts, `{"url":"`+rec.URL()+`","topic":"ops"}`)
	for _, topic := range []string{"ops", "marketing", "ops"} {
		postKnock(t, ts, `{"from":"a","topic":"`+topic+`"}`)
	}
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 2 {
		t.Errorf("attempts=%d, want 2 (ops-only)", got)
	}
}

func TestWebhookDelivery_RetriesOn5xx(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()
	rec.RespondWith(func(n int32, w http.ResponseWriter, _ *http.Request) {
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 5)

	registerWebhook(t, ts, `{"url":"`+rec.URL()+`"}`)
	postKnock(t, ts, `{"from":"a","topic":"x"}`)
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 3 {
		t.Errorf("attempts=%d, want 3 (two 5xx + one 200)", got)
	}
	// Final delivery attempt header should reflect retry count.
	last := rec.Received()[2]
	if last.headers.Get("X-Dingdong-Delivery-Attempt") != "3" {
		t.Errorf("last attempt header=%q, want 3", last.headers.Get("X-Dingdong-Delivery-Attempt"))
	}
}

func TestWebhookDelivery_RetriesOn429(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()
	rec.RespondWith(func(n int32, w http.ResponseWriter, _ *http.Request) {
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	registerWebhook(t, ts, `{"url":"`+rec.URL()+`"}`)
	postKnock(t, ts, `{"from":"a","topic":"x"}`)
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 2 {
		t.Errorf("attempts=%d, want 2 (429 then 200)", got)
	}
}

func TestWebhookDelivery_NoRetryOn4xx(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()
	rec.RespondWith(func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	registerWebhook(t, ts, `{"url":"`+rec.URL()+`"}`)
	postKnock(t, ts, `{"from":"a","topic":"x"}`)
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 1 {
		t.Errorf("attempts=%d, want 1 (4xx must not retry)", got)
	}
}

func TestWebhookDelivery_GivesUpAfterMaxAttempts(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()
	rec.RespondWith(func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 3)

	registerWebhook(t, ts, `{"url":"`+rec.URL()+`"}`)
	postKnock(t, ts, `{"from":"a","topic":"x"}`)
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 3 {
		t.Errorf("attempts=%d, want 3 (give up after MaxAttempts)", got)
	}
}

func TestWebhookDelivery_AfterDeleteStopsDelivery(t *testing.T) {
	rec := newReceiver()
	defer rec.Close()

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	sub := registerWebhook(t, ts, `{"url":"`+rec.URL()+`"}`)

	// Delete the registration
	resp := doRequest(t, bearerReq(t, http.MethodDelete, ts.URL+"/v1/webhooks/"+sub.ID, ""))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}

	postKnock(t, ts, `{"from":"a","topic":"x"}`)
	srv.WaitForDeliveries()

	if got := rec.AttemptCount(); got != 0 {
		t.Errorf("attempts=%d, want 0 (deleted subscriber must not be called)", got)
	}
}

func TestWebhookDelivery_MultipleSubscribers(t *testing.T) {
	r1 := newReceiver()
	defer r1.Close()
	r2 := newReceiver()
	defer r2.Close()

	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 4)

	registerWebhook(t, ts, `{"url":"`+r1.URL()+`","topic":"ops","secret":"s1"}`)
	registerWebhook(t, ts, `{"url":"`+r2.URL()+`","topic":"ops","secret":"s2"}`)
	postKnock(t, ts, `{"from":"a","topic":"ops","subject":"hi"}`)
	srv.WaitForDeliveries()

	if r1.AttemptCount() != 1 || r2.AttemptCount() != 1 {
		t.Errorf("attempts: r1=%d r2=%d, want 1/1", r1.AttemptCount(), r2.AttemptCount())
	}

	g1 := r1.Received()[0]
	g2 := r2.Received()[0]
	if g1.signature == g2.signature {
		t.Error("signatures should differ — different secrets sign the same body to different MACs")
	}
	if g1.signature != SignBody(g1.body, "s1") {
		t.Errorf("r1 signature mismatch")
	}
	if g2.signature != SignBody(g2.body, "s2") {
		t.Errorf("r2 signature mismatch")
	}
}

func TestWebhookDelivery_TransportErrorRetries(t *testing.T) {
	// Point at a URL that won't resolve to exercise the transport-error
	// branch. Use the standard reserved domain so we don't accidentally
	// hit anything real.
	ts, srv := newTestServer(t)
	withFastDispatcher(srv, 2)

	registerWebhook(t, ts, `{"url":"http://does-not-resolve.invalid./hook","topic":"ops"}`)
	postKnock(t, ts, `{"from":"a","topic":"ops"}`)
	// Should not block forever — Sleep is the no-op and the HTTP client
	// times out fast in test.
	srv.WaitForDeliveries()
	// No assertion on receiver; the point is WaitForDeliveries returns.
}

func TestStore_AddWebhook_GeneratesSecretWhenEmpty(t *testing.T) {
	s := NewMemStore(10)
	w := s.AddWebhook(Webhook{URL: "http://x.example/h"})
	if w.Secret == "" {
		t.Error("expected an auto-generated secret")
	}
	if len(w.Secret) < 32 {
		t.Errorf("auto-secret looks short: len=%d", len(w.Secret))
	}
}

func TestStore_AddWebhook_KeepsCallerSecret(t *testing.T) {
	s := NewMemStore(10)
	w := s.AddWebhook(Webhook{URL: "http://x.example/h", Secret: "mine"})
	if w.Secret != "mine" {
		t.Errorf("Secret=%q, want mine", w.Secret)
	}
}

func TestStore_MatchingWebhooks(t *testing.T) {
	s := NewMemStore(10)
	wildcard := s.AddWebhook(Webhook{URL: "http://x.example/h"})          // matches everything
	ops := s.AddWebhook(Webhook{URL: "http://y.example/h", Topic: "ops"}) // ops only
	mkt := s.AddWebhook(Webhook{URL: "http://z.example/h", Topic: "marketing"})

	got := s.MatchingWebhooks("ops")
	if len(got) != 2 {
		t.Fatalf("ops match len=%d, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, w := range got {
		seen[w.ID] = true
	}
	if !seen[wildcard.ID] || !seen[ops.ID] || seen[mkt.ID] {
		t.Errorf("matched IDs: %v; want wildcard+ops only", seen)
	}

	if got := s.MatchingWebhooks("nope"); len(got) != 1 || got[0].ID != wildcard.ID {
		t.Errorf("unknown topic match=%v, want [wildcard]", got)
	}
}

func TestStore_DeleteWebhook(t *testing.T) {
	s := NewMemStore(10)
	w1 := s.AddWebhook(Webhook{URL: "http://a/h"})
	w2 := s.AddWebhook(Webhook{URL: "http://b/h"})

	if !s.DeleteWebhook(w1.ID) {
		t.Error("delete returned false for existing webhook")
	}
	if s.DeleteWebhook("nonexistent") {
		t.Error("delete returned true for missing webhook")
	}
	remaining := s.ListWebhooks()
	if len(remaining) != 1 || remaining[0].ID != w2.ID {
		t.Errorf("after delete: %v, want only [%s]", remaining, w2.ID)
	}
}
