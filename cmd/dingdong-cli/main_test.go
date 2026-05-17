package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// streamerFunc adapts an ordinary function to the knockStreamer interface so
// tests can drive tailLoop without an HTTP server.
type streamerFunc func(ctx context.Context, cfg config, topic, to, since string, onKnock func(knock) bool) error

func (f streamerFunc) Stream(ctx context.Context, cfg config, topic, to, since string, onKnock func(knock) bool) error {
	return f(ctx, cfg, topic, to, since, onKnock)
}

// TestTailLoopReconnectsWithSince exercises the production tailLoop retry
// path directly via a fake knockStreamer. It asserts that after the first
// connection ends (clean EOF), tailLoop reconnects and passes the last-seen
// knock id as the since parameter. This is the genuine regression detector
// for issue #40 — earlier tests mirrored the loop inline instead of calling
// the production function, so a regression in the real loop could ship
// undetected.
func TestTailLoopReconnectsWithSince(t *testing.T) {
	var calls atomic.Int32
	sinceCh := make(chan string, 4)

	streamer := streamerFunc(func(ctx context.Context, _ config, topic, _, since string, onKnock func(knock) bool) error {
		n := calls.Add(1)
		select {
		case sinceCh <- since:
		default:
		}
		switch n {
		case 1:
			// First connection: deliver id001, then clean EOF triggers reconnect.
			onKnock(knock{ID: "id001", Topic: topic, Kind: "info"})
			return nil
		case 2:
			// Second connection: deliver id002, then block until ctx is cancelled.
			onKnock(knock{ID: "id002", Topic: topic, Kind: "info"})
			<-ctx.Done()
			return ctx.Err()
		default:
			<-ctx.Done()
			return ctx.Err()
		}
	})

	var out bytes.Buffer
	cfg := config{URL: "http://example.invalid", Token: "tok"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- tailLoop(ctx, streamer, cfg, "t", "", "", &out, tailOpts{
			Delays: []time.Duration{1 * time.Millisecond},
			ErrW:   io.Discard,
		})
	}()

	var firstSince, secondSince string
	select {
	case firstSince = <-sinceCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first call to streamer")
	}
	select {
	case secondSince = <-sinceCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnect")
	}

	if firstSince != "" {
		t.Errorf("first call since=%q, want empty", firstSince)
	}
	if secondSince != "id001" {
		t.Errorf("reconnect since=%q, want id001", secondSince)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("tailLoop returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(time.Second):
		t.Fatal("tailLoop did not return after ctx cancel")
	}

	body := out.String()
	if !strings.Contains(body, `"id":"id001"`) || !strings.Contains(body, `"id":"id002"`) {
		t.Errorf("writer output missing knocks; got: %q", body)
	}
}

// TestTailLoopNoRetryStopsOnFirstDrop verifies tailOpts.NoRetry causes the
// loop to return after one stream attempt rather than reconnecting.
func TestTailLoopNoRetryStopsOnFirstDrop(t *testing.T) {
	var calls atomic.Int32
	streamer := streamerFunc(func(_ context.Context, _ config, _, _, _ string, _ func(knock) bool) error {
		calls.Add(1)
		return nil // clean EOF
	})

	err := tailLoop(context.Background(), streamer, config{Token: "tok"}, "t", "", "", io.Discard, tailOpts{
		NoRetry: true,
		ErrW:    io.Discard,
	})
	if err != nil {
		t.Fatalf("tailLoop returned %v, want nil", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("streamer called %d times, want 1 (NoRetry)", got)
	}
}

// TestTailLoopPermanentErrorReturns verifies a permanentErr from the
// streamer propagates out of tailLoop without retry.
func TestTailLoopPermanentErrorReturns(t *testing.T) {
	var calls atomic.Int32
	streamer := streamerFunc(func(_ context.Context, _ config, _, _, _ string, _ func(knock) bool) error {
		calls.Add(1)
		return &permanentErr{errors.New("401")}
	})

	err := tailLoop(context.Background(), streamer, config{Token: "tok"}, "t", "", "", io.Discard, tailOpts{
		Delays: []time.Duration{time.Millisecond},
		ErrW:   io.Discard,
	})
	var perm *permanentErr
	if !errors.As(err, &perm) {
		t.Fatalf("tailLoop returned %T %v, want *permanentErr", err, err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("streamer called %d times, want 1 (no retry on permanent)", got)
	}
}

// sseEvent formats a single SSE knock event frame.
func sseEvent(k knock) string {
	b, _ := json.Marshal(k)
	return fmt.Sprintf("id: %s\nevent: knock\ndata: %s\n\n", k.ID, b)
}

// TestTailAutoReconnect verifies that streamKnocks is called again after a
// transient server-side EOF and that the reconnect request uses the last-seen
// knock ID as the since parameter.
func TestTailAutoReconnect(t *testing.T) {
	var connections atomic.Int32

	// sinceOnReconnect captures the since param sent on the second connection.
	sinceOnReconnect := make(chan string, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject unauthenticated requests.
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if auth != "tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		conn := connections.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		if conn == 1 {
			// First connection: emit one knock, then close (simulates pod rollout EOF).
			fmt.Fprint(w, sseEvent(knock{ID: "id001", From: "a", Topic: "t", Kind: "info"}))
			flusher.Flush()
			// Close the response — the client will see an unexpected EOF.
			return
		}

		// Second+ connection: capture the since param and emit one knock, then block
		// until the context is cancelled.
		sinceOnReconnect <- r.URL.Query().Get("since")
		fmt.Fprint(w, sseEvent(knock{ID: "id002", From: "a", Topic: "t", Kind: "info"}))
		flusher.Flush()
		// Hold the connection open until client disconnects.
		<-r.Context().Done()
	}))
	defer ts.Close()

	cfg := config{URL: ts.URL, Token: "tok"}
	var mu sync.Mutex
	var received []string

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	streamErr := make(chan error, 1)
	lastSeen := ""
	attempt := 0

	// Mirror the retry loop from runTail so we can test it with a short delay.
	testDelays := []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	go func() {
		for {
			err := streamKnocks(ctx, cfg, "t", "", lastSeen, func(k knock) bool {
				mu.Lock()
				received = append(received, k.ID)
				if k.ID != "" {
					lastSeen = k.ID
				}
				mu.Unlock()
				return true
			})

			// Only stop on context cancellation; nil (clean EOF) triggers reconnect.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				streamErr <- nil
				return
			}
			var perm *permanentErr
			if errors.As(err, &perm) {
				streamErr <- err
				return
			}

			delay := testDelays[min(attempt, len(testDelays)-1)]
			attempt++
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				streamErr <- nil
				return
			}
		}
	}()

	// Wait until we see the since on reconnect.
	var gotSince string
	select {
	case gotSince = <-sinceOnReconnect:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnection")
	}

	// Cancel to stop the second connection.
	cancel()
	<-streamErr

	if gotSince != "id001" {
		t.Errorf("reconnect since=%q, want id001", gotSince)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 1 || received[0] != "id001" {
		t.Errorf("first received=%v, want id001 as first element", received)
	}
}

// TestTailPermanentErrorNoRetry verifies that streamKnocks returns a
// permanentErr on 401 and that the retry loop does not attempt to reconnect.
func TestTailPermanentErrorNoRetry(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts.Close()

	cfg := config{URL: ts.URL, Token: "bad-tok"}
	ctx := context.Background()

	err := streamKnocks(ctx, cfg, "", "", "", func(k knock) bool { return true })
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var perm *permanentErr
	if !errors.As(err, &perm) {
		t.Errorf("expected permanentErr for 401, got %T: %v", err, err)
	}

	if calls != 1 {
		t.Errorf("server called %d times, want 1 (no retry on permanent error)", calls)
	}
}

// TestStreamKnocksTracksLastSeen verifies that streamKnocks returns scanner
// errors (non-permanent) so the retry loop can reconnect.
func TestTailReturnsTransientErrOnEOF(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Close immediately — simulates EOF.
	}))
	defer ts.Close()

	cfg := config{URL: ts.URL, Token: "tok"}
	ctx := context.Background()

	err := streamKnocks(ctx, cfg, "", "", "", func(k knock) bool { return true })
	// Should return nil or a non-permanent error (EOF returns nil from scanner).
	// Either way it must NOT be a permanentErr.
	var perm *permanentErr
	if errors.As(err, &perm) {
		t.Errorf("EOF should not produce permanentErr, got: %v", err)
	}

	// Reading an empty body is not an error in the scanner; it returns nil.
	// The retry loop in runTail treats nil as a transient drop and reconnects.
	// This test only verifies that no permanentErr is returned.
	_ = err
}

// TestStreamKnocksReadsBody exercises the basic happy path of streamKnocks to
// ensure the SSE parser still works after the permanentErr refactor.
func TestStreamKnocksReadsBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		k := knock{ID: "id001", From: "a", Topic: "t", Kind: "info"}
		fmt.Fprint(w, sseEvent(k))
		// Closing body triggers EOF → streamKnocks returns.
	}))
	defer ts.Close()

	cfg := config{URL: ts.URL, Token: "tok"}

	var got []string
	ctx := context.Background()

	// Use a custom client that has auth in the right place.
	// streamKnocks reads token from cfg.Token and sets the Authorization header.
	// But the test server doesn't check auth, so this works as-is.
	err := streamKnocks(ctx, cfg, "", "", "", func(k knock) bool {
		got = append(got, k.ID)
		return true
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "id001" {
		t.Errorf("received=%v, want [id001]", got)
	}
}

// TestStreamKnocksUsesAuthHeader verifies that streamKnocks passes the token
// as a ?token= query param (for EventSource compat) — regression for auth.
func TestStreamKnocksUsesTokenParam(t *testing.T) {
	var gotToken string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.URL.Query().Get("token")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, ": keepalive\n\n")
	}))
	defer ts.Close()

	cfg := config{URL: ts.URL, Token: "mytoken"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_ = streamKnocks(ctx, cfg, "", "", "", func(k knock) bool { return true })

	if gotToken != "mytoken" {
		t.Errorf("token param=%q, want mytoken", gotToken)
	}
}
