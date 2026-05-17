package server

import (
	"encoding/json"
	"net/http"
)

func (s *Server) handleListTopics(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Topics()
	if err != nil {
		http.Error(w, "store unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
