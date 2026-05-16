package server

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// defaultTopic is the channel name used in two places: Topics() always surfaces
// it so the UI sidebar has a stable anchor even on a fresh server, and
// handlePostKnock falls back to it when a knock arrives with topic="".
const defaultTopic = "main"

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

// Backend is the durability port. Implementations persist knocks and answer
// historical queries; the in-memory subscriber hub on Store handles live
// fan-out separately. Backends are responsible for their own concurrency.
type Backend interface {
	Save(Knock) error
	Query(f Filter, since string, limit int) ([]Knock, error)
	Topics() ([]string, error)
	Close() error
}

type subscription struct {
	ch chan Knock
}

// Store composes the live subscriber hub with a Backend. Add is held under
// s.mu through both backend.Save and the fan-out loop so subscribers receive
// events in the same order they're persisted, and so a concurrent cancel()
// cannot close a subscriber channel between snapshot and send.
type Store struct {
	mu       sync.Mutex
	subs     map[*subscription]struct{}
	backend  Backend
	webhooks []Webhook
}

func NewStore(backend Backend) *Store {
	return &Store{
		subs:    make(map[*subscription]struct{}),
		backend: backend,
	}
}

// NewMemStore is a convenience constructor for tests and local development
// that wraps a ring-buffer-backed Backend. Production callers should build
// a durable backend (e.g., the SQLite one) and pass it to NewStore directly.
func NewMemStore(cap int) *Store {
	return NewStore(newMemBackend(cap))
}

func (s *Store) Add(k Knock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.backend.Save(k); err != nil {
		return err
	}
	for sub := range s.subs {
		select {
		case sub.ch <- k:
		default:
			// Subscriber is slow; drop. They can recover via GET /v1/knocks?since=...
		}
	}
	return nil
}

func (s *Store) List(f Filter, since string, limit int) ([]Knock, error) {
	return s.backend.Query(f, since, limit)
}

func (s *Store) Topics() ([]string, error) {
	return s.backend.Topics()
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

// Close releases backend resources and closes all live subscriber channels
// so SSE handlers unblock and return. backend.Close runs inside s.mu so a
// concurrent Add can't sneak a Save in between subscriber teardown and the
// backend shutdown (sqliteBackend.Close uses sync.Once and takes no
// store-level locks, so this won't deadlock).
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		delete(s.subs, sub)
		close(sub.ch)
	}
	return s.backend.Close()
}

// memBackend is a ring-buffer-backed Backend. Used for tests and local
// development where durability is not needed.
type memBackend struct {
	mu    sync.Mutex
	cap   int
	items []Knock
}

func newMemBackend(cap int) *memBackend {
	if cap <= 0 {
		cap = 1000
	}
	return &memBackend{cap: cap}
}

func (b *memBackend) Save(k Knock) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items = append(b.items, k)
	if len(b.items) > b.cap {
		b.items = b.items[len(b.items)-b.cap:]
	}
	return nil
}

func (b *memBackend) Query(f Filter, since string, limit int) ([]Knock, error) {
	if limit <= 0 {
		limit = 100
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Knock, 0, limit)
	for _, k := range b.items {
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
	return out, nil
}

func (b *memBackend) Topics() ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	seen := map[string]struct{}{defaultTopic: {}}
	for _, k := range b.items {
		if k.Topic == "" {
			continue
		}
		seen[k.Topic] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

func (b *memBackend) Close() error { return nil }

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
