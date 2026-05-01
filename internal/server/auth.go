package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearerOrQuery(r)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dingdong"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// bearerOrQuery accepts the token from `Authorization: Bearer ...` or `?token=...`.
// Query support is for browsers (EventSource) and curl convenience; both paths
// go over TLS in production.
func bearerOrQuery(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return r.URL.Query().Get("token")
}
