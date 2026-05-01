package server

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"
)

type Knock struct {
	ID        string    `json:"id"`
	Ts        time.Time `json:"ts"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Topic     string    `json:"topic"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject,omitempty"`
	Body      string    `json:"body,omitempty"`
	InReplyTo string    `json:"in_reply_to,omitempty"`
}

type Filter struct {
	Topic string
	To    string
}

func (f Filter) Match(k Knock) bool {
	if f.Topic != "" && k.Topic != f.Topic {
		return false
	}
	// If subscriber filters by `to`, they want directed messages addressed to
	// them plus broadcasts (k.To == ""). Reject only when both are set and differ.
	if f.To != "" && k.To != "" && k.To != f.To {
		return false
	}
	return true
}

type subscription struct {
	ch chan Knock
}

type Store struct {
	mu    sync.Mutex
	cap   int
	items []Knock
	subs  map[*subscription]struct{}
}

func NewStore(cap int) *Store {
	return &Store{
		cap:  cap,
		subs: make(map[*subscription]struct{}),
	}
}

func (s *Store) Add(k Knock) {
	s.mu.Lock()
	s.items = append(s.items, k)
	if len(s.items) > s.cap {
		s.items = s.items[len(s.items)-s.cap:]
	}
	subs := make([]*subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- k:
		default:
			// Subscriber is slow; drop. They can recover via GET /v1/knocks?since=...
		}
	}
}

func (s *Store) List(f Filter, since string, limit int) []Knock {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Knock, 0, limit)
	for _, k := range s.items {
		if since != "" && k.ID <= since {
			continue
		}
		if !f.Match(k) {
			continue
		}
		out = append(out, k)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Store) Subscribe() (<-chan Knock, func()) {
	sub := &subscription{ch: make(chan Knock, 64)}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	cancel := func() {
		s.mu.Lock()
		if _, ok := s.subs[sub]; ok {
			delete(s.subs, sub)
			close(sub.ch)
		}
		s.mu.Unlock()
	}
	return sub.ch, cancel
}

// NewID returns a 28-char hex id whose lexicographic order matches creation
// order: 8 bytes of unix-nanos big-endian + 6 random bytes for tiebreaks.
func NewID() string {
	var b [14]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
	if _, err := rand.Read(b[8:]); err != nil {
		// crypto/rand should not fail; fall back to time-only suffix
		binary.BigEndian.PutUint32(b[8:], uint32(time.Now().Nanosecond()))
	}
	return hex.EncodeToString(b[:])
}
