package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	f := Filter{Topic: q.Get("topic"), To: q.Get("to")}
	since := q.Get("since")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe before draining backlog so we can't miss events that land in
	// between. Duplicates are filtered by id below.
	ch, cancel := s.store.Subscribe()
	defer cancel()

	for _, k := range s.store.List(f, since, 1000) {
		writeSSE(w, k)
		since = k.ID
	}
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case k, ok := <-ch:
			if !ok {
				return
			}
			if since != "" && k.ID <= since {
				continue
			}
			if !f.Match(k) {
				continue
			}
			writeSSE(w, k)
			flusher.Flush()
			since = k.ID
		}
	}
}

func writeSSE(w io.Writer, k Knock) {
	b, err := json.Marshal(k)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %s\nevent: knock\ndata: %s\n\n", k.ID, b)
}
