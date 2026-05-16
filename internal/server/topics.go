package server

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleListTopics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.store.Topics())
}
