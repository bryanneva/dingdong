package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStream_HeadersAndBacklog(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.Add(Knock{ID: "id001", From: "a", Topic: "ops"})
	srv.store.Add(Knock{ID: "id002", From: "b", Topic: "ops"})

	resp, cancel := openStream(t, ts.URL+"/v1/stream")
	defer cancel()

	if got := resp.StatusCode; got != http.StatusOK {
		t.Fatalf("status=%d", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type=%q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control=%q, want no-cache", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering=%q, want no", got)
	}

	frames := streamFrames(resp.Body)
	got := readKnocks(t, frames, 2, 2*time.Second)
	if got[0].ID != "id001" || got[1].ID != "id002" {
		t.Errorf("backlog ids=%v, want [id001 id002]", ids(got))
	}
}

func TestStream_SinceResume(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.Add(Knock{ID: "id001", From: "a", Topic: "ops"})
	srv.store.Add(Knock{ID: "id002", From: "a", Topic: "ops"})
	srv.store.Add(Knock{ID: "id003", From: "a", Topic: "ops"})

	resp, cancel := openStream(t, ts.URL+"/v1/stream?since=id002")
	defer cancel()

	frames := streamFrames(resp.Body)
	got := readKnocks(t, frames, 1, 2*time.Second)
	if got[0].ID != "id003" {
		t.Errorf("since=id002: id=%s, want id003 (since is exclusive)", got[0].ID)
	}
}

func TestStream_TopicFilter(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.Add(Knock{ID: "id001", From: "a", Topic: "ops"})
	srv.store.Add(Knock{ID: "id002", From: "a", Topic: "marketing"})
	srv.store.Add(Knock{ID: "id003", From: "a", Topic: "ops"})

	resp, cancel := openStream(t, ts.URL+"/v1/stream?topic=ops")
	defer cancel()

	frames := streamFrames(resp.Body)
	got := readKnocks(t, frames, 2, 2*time.Second)
	if got[0].ID != "id001" || got[1].ID != "id003" {
		t.Errorf("topic=ops: ids=%v, want [id001 id003]", ids(got))
	}
}

func TestStream_LiveDelivery(t *testing.T) {
	ts, srv := newTestServer(t)
	// IDs must be lex-ordered: real NewID() outputs are time-sorted, and the
	// stream handler's de-dup uses k.ID <= since. The sentinel proves the
	// handler completed Subscribe + backlog drain before we push live events.
	srv.store.Add(Knock{ID: "id001-sentinel", From: "a", Topic: "ops"})

	resp, cancel := openStream(t, ts.URL+"/v1/stream")
	defer cancel()

	frames := streamFrames(resp.Body)
	got := readKnocks(t, frames, 1, 2*time.Second)
	if got[0].ID != "id001-sentinel" {
		t.Fatalf("sentinel id=%s", got[0].ID)
	}

	srv.store.Add(Knock{ID: "id002-live", From: "a", Topic: "ops"})
	got = readKnocks(t, frames, 1, 2*time.Second)
	if got[0].ID != "id002-live" {
		t.Errorf("live id=%s, want id002-live", got[0].ID)
	}
}

func TestStream_SSEFrameFormat(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.Add(Knock{ID: "abc123", From: "a", Topic: "ops", Subject: "hi"})

	resp, cancel := openStream(t, ts.URL+"/v1/stream")
	defer cancel()

	frames := streamFrames(resp.Body)
	frame := readFrame(t, frames, 2*time.Second)

	if !strings.HasPrefix(frame, "id: abc123\n") {
		t.Errorf("frame missing or misformatted id line:\n%s", frame)
	}
	if !strings.Contains(frame, "\nevent: knock\n") {
		t.Errorf("frame missing event line:\n%s", frame)
	}
	if !strings.Contains(frame, `"id":"abc123"`) || !strings.Contains(frame, `"subject":"hi"`) {
		t.Errorf("data line missing knock fields:\n%s", frame)
	}
}

func TestStream_ClientCancelClosesGracefully(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.Add(Knock{ID: "warmup", From: "a", Topic: "ops"})

	resp, cancel := openStream(t, ts.URL+"/v1/stream")
	frames := streamFrames(resp.Body)

	// Drain warmup so we know the handler is in the live loop.
	_ = readFrame(t, frames, 2*time.Second)

	// Cancel the request context — handler should hit ctx.Done() and return,
	// which closes the connection and EOFs the client read, which closes the
	// frames channel.
	cancel()

	closed := make(chan struct{})
	go func() {
		for range frames {
		}
		close(closed)
	}()
	select {
	case <-closed:
		// stream closed cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("frames did not close after ctx cancel")
	}
}

// openStream is a stream-specific request helper: builds the Bearer-authed
// request, attaches a cancelable context, and returns both. Caller must call
// cancel() (defer or t.Cleanup) to release the connection.
func openStream(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	req := bearerReq(t, http.MethodGet, url, "")
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, cancel
}

// streamFrames reads SSE response bytes and emits each frame's text (the
// content between blank-line separators) on the returned channel. Closes the
// channel when the body returns EOF.
func streamFrames(body io.Reader) <-chan string {
	out := make(chan string, 16)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		scanner.Split(splitOnDoubleNewline)
		for scanner.Scan() {
			out <- scanner.Text()
		}
	}()
	return out
}

// splitOnDoubleNewline emits a token whenever it sees "\n\n" in the byte
// stream. Used as the SplitFunc for parsing SSE frames.
func splitOnDoubleNewline(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func frameToKnock(frame string) (Knock, bool) {
	for _, line := range strings.Split(frame, "\n") {
		rest, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var k Knock
		if err := json.Unmarshal([]byte(rest), &k); err != nil {
			return Knock{}, false
		}
		return k, true
	}
	return Knock{}, false
}

func readFrame(t *testing.T, frames <-chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("frames closed before frame received")
		}
		return f
	case <-time.After(timeout):
		t.Fatalf("no frame in %v", timeout)
		return ""
	}
}

func readKnocks(t *testing.T, frames <-chan string, n int, timeout time.Duration) []Knock {
	t.Helper()
	out := make([]Knock, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(out) < n {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatalf("frames closed after %d/%d knocks", len(out), n)
			}
			if k, ok := frameToKnock(f); ok {
				out = append(out, k)
			}
		case <-deadline.C:
			t.Fatalf("timeout after %d/%d knocks", len(out), n)
		}
	}
	return out
}
