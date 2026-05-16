package server

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestListTopics_HTTP(t *testing.T) {
	ts, srv := newTestServer(t)
	srv.store.Add(Knock{ID: "id001", From: "a", Topic: "ops"})
	srv.store.Add(Knock{ID: "id002", From: "a", Topic: "agents"})
	srv.store.Add(Knock{ID: "id003", From: "a", Topic: "ops"})

	resp := doRequest(t, bearerReq(t, http.MethodGet, ts.URL+"/v1/topics", ""))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, buf)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}

	var got []string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := map[string]bool{"main": true, "ops": true, "agents": true}
	if len(got) != len(want) {
		t.Fatalf("topics=%v, want %v keys", got, want)
	}
	for _, topic := range got {
		if !want[topic] {
			t.Errorf("unexpected topic %q in %v", topic, got)
		}
	}
}

func TestListTopics_EmptyStoreStillReturnsMain(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodGet, ts.URL+"/v1/topics", ""))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got []string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0] != "main" {
		t.Errorf("empty store /v1/topics=%v, want [main]", got)
	}
}

func TestListTopics_RequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/topics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}
