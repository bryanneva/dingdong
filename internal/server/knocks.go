package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// maxKnockBytes caps POST /v1/knocks bodies. Knocks are short coordination
// messages — a few hundred bytes is typical, an oversize body almost always
// indicates a misbehaving client or accidental file upload.
const maxKnockBytes = 1 << 20 // 1 MiB

func (s *Server) handlePostKnock(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxKnockBytes)
	var k Knock
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
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
		k.Topic = defaultTopic
	}
	if k.Kind == "" {
		k.Kind = "info"
	}
	if err := s.store.Add(k); err != nil {
		http.Error(w, "store unavailable", http.StatusInternalServerError)
		return
	}
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
	out, err := s.store.List(f, since, limit)
	if err != nil {
		http.Error(w, "store unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
