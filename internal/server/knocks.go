package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

func (s *Server) handlePostKnock(w http.ResponseWriter, r *http.Request) {
	var k Knock
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if k.From == "" {
		http.Error(w, "from is required", http.StatusBadRequest)
		return
	}
	k.ID = NewID()
	k.Ts = time.Now().UTC()
	if k.Topic == "" {
		k.Topic = "default"
	}
	if k.Kind == "" {
		k.Kind = "info"
	}
	s.store.Add(k)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(k)
}

func (s *Server) handleListKnocks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{Topic: q.Get("topic"), To: q.Get("to")}
	since := q.Get("since")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 {
		limit = 100
	}
	out := s.store.List(f, since, limit)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
