package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireAuth(t *testing.T) {
	srv := New(Config{Token: testToken})
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
	mw := srv.requireAuth(stub)

	tests := []struct {
		name        string
		header      string // Authorization header value (empty = unset)
		query       string // ?token= value (empty = no query param)
		wantStatus  int
		wantWWWAuth bool
	}{
		{"correct bearer header", "Bearer " + testToken, "", http.StatusOK, false},
		{"wrong bearer header", "Bearer wrong", "", http.StatusUnauthorized, true},
		{"correct query token", "", testToken, http.StatusOK, false},
		{"wrong query token", "", "wrong", http.StatusUnauthorized, true},
		{"no auth at all", "", "", http.StatusUnauthorized, true},
		{"basic header falls through to query token", "Basic dXNlcjpwYXNz", testToken, http.StatusOK, false},
		{"basic header falls through, no query", "Basic dXNlcjpwYXNz", "", http.StatusUnauthorized, true},
		{"bearer header takes precedence — wrong header beats valid query", "Bearer wrong", testToken, http.StatusUnauthorized, true},
		{"bearer with surrounding whitespace is trimmed", "Bearer   " + testToken + "  ", "", http.StatusOK, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/v1/knocks"
			if tt.query != "" {
				url += "?token=" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			gotWWW := rec.Header().Get("WWW-Authenticate")
			if tt.wantWWWAuth {
				if gotWWW != `Bearer realm="dingdong"` {
					t.Errorf(`WWW-Authenticate = %q, want Bearer realm="dingdong"`, gotWWW)
				}
			} else if gotWWW != "" {
				t.Errorf("WWW-Authenticate should be empty on success, got %q", gotWWW)
			}
		})
	}
}
