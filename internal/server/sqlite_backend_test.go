package server

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSQLiteBackend_DurabilityAcrossReopen is the load-bearing test for
// issue #53: knocks survive close + reopen of the backend. This is the
// behavioral promise that motivates the whole persistence layer.
func TestSQLiteBackend_DurabilityAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	b1, err := newSQLiteBackend(path, 1000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := b1.Save(Knock{ID: "id001", Topic: "ops", From: "alice", Ts: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if err := b1.Save(Knock{ID: "id002", Topic: "ops", From: "bob", Ts: time.Unix(2, 0).UTC()}); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the same file.
	b2, err := newSQLiteBackend(path, 1000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b2.Close()

	got, err := b2.Query(Filter{}, "", 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after reopen: got %d rows (%v), want 2", len(got), ids(got))
	}
	if got[0].ID != "id001" || got[1].ID != "id002" {
		t.Errorf("after reopen: ids=%v, want [id001 id002]", ids(got))
	}
	if got[0].From != "alice" || got[1].From != "bob" {
		t.Errorf("from round-trip: got [%s, %s], want [alice, bob]", got[0].From, got[1].From)
	}
}

// TestSQLiteBackend_RetentionTrimsOldest exercises the count-based retention
// policy: when retentionRows=5 and we save 10 knocks, trim() leaves the
// newest 5. Verifies the operational bound on disk usage.
func TestSQLiteBackend_RetentionTrimsOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	b, err := newSQLiteBackend(path, 5)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	for i := 1; i <= 10; i++ {
		if err := b.Save(Knock{ID: pad(i), Topic: "ops"}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	if err := b.trim(); err != nil {
		t.Fatalf("trim: %v", err)
	}

	got, err := b.Query(Filter{}, "", 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("after trim: got %d rows (%v), want 5", len(got), ids(got))
	}
	// Newest 5 should remain: pad(6)..pad(10) in ascending id order.
	for i, k := range got {
		want := pad(6 + i)
		if k.ID != want {
			t.Errorf("position %d: id=%s, want %s", i, k.ID, want)
		}
	}
}

// TestSQLiteBackend_ReservedWordSafety proves the column-rename (from/to →
// from_id/to_id) doesn't leak through the wire format. SQL keywords as
// column names are a portability footgun; the schema sidesteps it and the
// JSON tags keep the API contract intact.
func TestSQLiteBackend_ReservedWordSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	b, err := newSQLiteBackend(path, 1000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	in := Knock{
		ID: "id001", Topic: "ops",
		From: "alice@host", To: "bob@host",
		Subject: "hi", Body: "body text",
		InReplyTo: "id000",
		Kind:      "info",
		Ts:        time.Unix(1700000000, 0).UTC(),
	}
	if err := b.Save(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := b.Query(Filter{}, "", 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	k := got[0]
	if k.From != in.From || k.To != in.To {
		t.Errorf("from/to round-trip: from=%q to=%q (want %q, %q)", k.From, k.To, in.From, in.To)
	}
	if k.Subject != in.Subject || k.Body != in.Body || k.InReplyTo != in.InReplyTo || k.Kind != in.Kind {
		t.Errorf("field round-trip: got %+v, want %+v", k, in)
	}
	if !k.Ts.Equal(in.Ts) {
		t.Errorf("ts round-trip: got %v, want %v", k.Ts, in.Ts)
	}
}

// TestSQLiteBackend_FilterParity asserts that sqliteBackend and memBackend
// produce identical Query results given identical inputs. This is the
// behavioral contract that lets the production wire-up swap backends
// without changing handler semantics.
func TestSQLiteBackend_FilterParity(t *testing.T) {
	dir := t.TempDir()
	sqlitePath := filepath.Join(dir, "parity.db")
	sb, err := newSQLiteBackend(sqlitePath, 1000)
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	defer sb.Close()
	mb := newMemBackend(1000)

	backends := map[string]Backend{"mem": mb, "sqlite": sb}
	for _, b := range backends {
		_ = b.Save(Knock{ID: "id001", Topic: "ops", To: "alice"})
		_ = b.Save(Knock{ID: "id002", Topic: "marketing", To: "alice"})
		_ = b.Save(Knock{ID: "id003", Topic: "ops", To: ""})
		_ = b.Save(Knock{ID: "id004", Topic: "ops", To: "bob"})
	}

	cases := []struct {
		name    string
		f       Filter
		since   string
		limit   int
		wantIDs []string
	}{
		{"all", Filter{}, "", 100, []string{"id001", "id002", "id003", "id004"}},
		{"since exclusive", Filter{}, "id002", 100, []string{"id003", "id004"}},
		{"topic=ops", Filter{Topic: "ops"}, "", 100, []string{"id001", "id003", "id004"}},
		{"to=alice includes broadcast", Filter{To: "alice"}, "", 100, []string{"id001", "id002", "id003"}},
		{"topic+to combined", Filter{Topic: "ops", To: "alice"}, "", 100, []string{"id001", "id003"}},
		{"limit caps", Filter{}, "", 2, []string{"id001", "id002"}},
		{"limit<=0 defaults to 100", Filter{}, "", 0, []string{"id001", "id002", "id003", "id004"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, b := range backends {
				got, err := b.Query(tc.f, tc.since, tc.limit)
				if err != nil {
					t.Fatalf("%s query: %v", name, err)
				}
				gotIDs := ids(got)
				if !equalStrings(gotIDs, tc.wantIDs) {
					t.Errorf("%s: ids=%v, want %v", name, gotIDs, tc.wantIDs)
				}
			}
		})
	}
}

// TestSQLiteBackend_TopicsIncludesMain mirrors the memBackend semantics:
// "main" is always present even on an empty store; empty topics are
// filtered; results are sorted.
func TestSQLiteBackend_TopicsIncludesMain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	b, err := newSQLiteBackend(path, 1000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	// Empty store still surfaces "main".
	got, err := b.Topics()
	if err != nil {
		t.Fatalf("topics: %v", err)
	}
	if len(got) != 1 || got[0] != "main" {
		t.Errorf("empty Topics()=%v, want [main]", got)
	}

	_ = b.Save(Knock{ID: "id001", Topic: "ops"})
	_ = b.Save(Knock{ID: "id002", Topic: "agents"})
	_ = b.Save(Knock{ID: "id003", Topic: "ops"})
	_ = b.Save(Knock{ID: "id004", Topic: ""}) // empty topic filtered

	got, err = b.Topics()
	if err != nil {
		t.Fatalf("topics: %v", err)
	}
	want := []string{"agents", "main", "ops"}
	if !equalStrings(got, want) {
		t.Errorf("Topics()=%v, want %v", got, want)
	}
}

// TestSQLiteBackend_RaceSaveQuery drives concurrent producers + a reader
// against the backend; -race must come back clean.
func TestSQLiteBackend_RaceSaveQuery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	b, err := newSQLiteBackend(path, 10000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()

	const producers = 5
	const itemsPerProducer = 50

	var wg sync.WaitGroup
	wg.Add(producers + 1)

	for p := 0; p < producers; p++ {
		go func(p int) {
			defer wg.Done()
			for i := 0; i < itemsPerProducer; i++ {
				_ = b.Save(Knock{ID: pad(p*itemsPerProducer + i), Topic: "t"})
			}
		}(p)
	}

	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_, _ = b.Query(Filter{}, "", 100)
		}
	}()

	wg.Wait()
}

// TestSQLiteBackend_CloseIsIdempotent guards against double-close panics
// when graceful shutdown races with an explicit Close in tests.
func TestSQLiteBackend_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	b, err := newSQLiteBackend(path, 1000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second close must not panic and should return nil.
	if err := b.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// TestSQLiteBackend_ConcurrentSaveClose drives concurrent Save calls through a
// b.Close() to prove two guarantees:
//  1. No panic — db.Exec after db.Close returns an error, not a segfault.
//     With SetMaxOpenConns(1) the pool serialises Save and Close, so this is a
//     structural ordering race rather than a Go data race.
//  2. Error shape contract — every non-nil Save error is either:
//     - sql.ErrConnDone ("sql: connection is already closed"), or
//     - "sql: database is closed" (db.Exec after db.Close, unexported sentinel)
//     Anything outside this set indicates unexpected behaviour.
//
// Run under -race to catch any Go-level data race in addition to the above.
func TestSQLiteBackend_ConcurrentSaveClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	b, err := newSQLiteBackend(path, 10000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const producers = 8
	const itemsPerProducer = 100

	errs := make(chan error, producers*itemsPerProducer)
	var wg sync.WaitGroup

	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < itemsPerProducer; i++ {
				errs <- b.Save(Knock{
					ID:    fmt.Sprintf("p%04d-%04d", p, i),
					Topic: "concurrent",
				})
			}
		}(p)
	}

	// Close mid-flight: goroutine startup is cheap, so Close likely arrives
	// while at least some producer Saves are still in flight.
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	wg.Wait()
	close(errs)

	for saveErr := range errs {
		if saveErr == nil {
			continue // Save completed before Close — fine.
		}
		if errors.Is(saveErr, sql.ErrConnDone) {
			continue // connection already closed — expected post-Close shape.
		}
		msg := saveErr.Error()
		if strings.Contains(msg, "database is closed") ||
			strings.Contains(msg, "connection is already closed") {
			continue // expected post-Close error text from database/sql internals.
		}
		t.Errorf("unexpected Save error: %v", saveErr)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
