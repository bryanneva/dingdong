package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestNew_DefaultsCap(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero defaults to 1000", 0, 1000},
		{"negative defaults to 1000", -1, 1000},
		{"explicit value preserved", 42, 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{Token: testToken, Cap: tt.in})
			if s.store.cap != tt.want {
				t.Errorf("store.cap = %d, want %d", s.store.cap, tt.want)
			}
		})
	}
}

func TestHealthz_Unauthenticated(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

func TestRoot_ServesStaticUnauthenticated(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "<html") && !strings.Contains(lower, "<!doctype") {
		t.Errorf("body should be HTML, got first 200 bytes: %.200q", body)
	}
}

func TestProtectedRoutesRequireAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, path := range []string{"/v1/knocks", "/v1/stream"} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}
