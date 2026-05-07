package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "test-token"

// newTestServer returns an httptest.Server backed by a real *Server. The
// server is closed via t.Cleanup. Tests that need to seed state directly
// (e.g. push to s.store.Add) can use the second return value.
func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	srv := New(Config{Token: testToken, Cap: 1000})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, srv
}

func bearerReq(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var br io.Reader
	if body != "" {
		br = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, br)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}
