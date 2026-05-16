package server

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; chosen so the distroless image stays CGO_ENABLED=0
)

// sqliteBackend is the durable Backend implementation. One DB file, WAL mode,
// single writer connection. Retention is enforced by an hourly trim goroutine.
type sqliteBackend struct {
	db            *sql.DB
	retentionRows int

	closeOnce sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// from/to are reserved-ish SQL keywords; renamed to from_id/to_id at the
// column layer. JSON tags on Knock stay as from/to so the wire format is
// unchanged.
const sqliteSchema = `
CREATE TABLE IF NOT EXISTS knocks (
    id          TEXT PRIMARY KEY,
    ts          INTEGER NOT NULL,
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL DEFAULT '',
    topic       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    subject     TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    in_reply_to TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX IF NOT EXISTS idx_knocks_topic_id ON knocks(topic, id);
`

// trimInterval is the cadence at which the retention loop fires. Hourly is
// plenty for a service with at most ~100 knocks/min peak.
const trimInterval = 1 * time.Hour

func newSQLiteBackend(path string, retentionRows int) (*sqliteBackend, error) {
	if retentionRows <= 0 {
		retentionRows = 100000
	}
	// WAL + synchronous=NORMAL is the standard tradeoff for a single-pod
	// service: crash-safe (durable on fsync at checkpoint), no per-commit
	// fsync cost. busy_timeout backs off briefly on lock contention.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %s: %w", path, err)
	}
	// Cap connections at 1: SQLite is single-writer regardless, and a
	// connection pool of 1 sidesteps the SQLITE_BUSY surprise that a fresh
	// connection would hit before busy_timeout kicks in.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	b := &sqliteBackend{
		db:            db,
		retentionRows: retentionRows,
		cancel:        cancel,
	}
	b.wg.Add(1)
	go b.trimLoop(ctx)
	return b, nil
}

func (b *sqliteBackend) Save(k Knock) error {
	_, err := b.db.Exec(
		`INSERT INTO knocks (id, ts, from_id, to_id, topic, kind, subject, body, in_reply_to)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Ts.UnixNano(), k.From, k.To, k.Topic, k.Kind, k.Subject, k.Body, k.InReplyTo,
	)
	if err != nil {
		return fmt.Errorf("save: %w", err)
	}
	return nil
}

func (b *sqliteBackend) Query(f Filter, since string, limit int) ([]Knock, error) {
	if limit <= 0 {
		limit = 100
	}
	// Filters are pushed into SQL so LIMIT is honored against post-filter
	// rows. The broadcast-inclusive `to` semantics (matches directed +
	// broadcast knocks) becomes (to_id = ? OR to_id = '').
	q := `SELECT id, ts, from_id, to_id, topic, kind, subject, body, in_reply_to FROM knocks WHERE 1=1`
	args := make([]any, 0, 4)
	if f.Topic != "" {
		q += " AND topic = ?"
		args = append(args, f.Topic)
	}
	if f.To != "" {
		q += " AND (to_id = ? OR to_id = '')"
		args = append(args, f.To)
	}
	if since != "" {
		q += " AND id > ?"
		args = append(args, since)
	}
	q += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := b.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Knock, 0, limit)
	for rows.Next() {
		var k Knock
		var tsNanos int64
		if err := rows.Scan(&k.ID, &tsNanos, &k.From, &k.To, &k.Topic, &k.Kind, &k.Subject, &k.Body, &k.InReplyTo); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		k.Ts = time.Unix(0, tsNanos).UTC()
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func (b *sqliteBackend) Topics() ([]string, error) {
	rows, err := b.db.Query(`SELECT DISTINCT topic FROM knocks WHERE topic != ''`)
	if err != nil {
		return nil, fmt.Errorf("topics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]struct{}{defaultTopic: {}}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("topics scan: %w", err)
		}
		seen[t] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topics rows: %w", err)
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// trim deletes everything older than the newest retentionRows by id. Exposed
// as a method so tests can force it synchronously without waiting for the
// ticker.
func (b *sqliteBackend) trim() error {
	_, err := b.db.Exec(
		`DELETE FROM knocks WHERE id NOT IN (SELECT id FROM knocks ORDER BY id DESC LIMIT ?)`,
		b.retentionRows,
	)
	if err != nil {
		return fmt.Errorf("trim: %w", err)
	}
	return nil
}

func (b *sqliteBackend) trimLoop(ctx context.Context) {
	defer b.wg.Done()
	t := time.NewTicker(trimInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Best-effort; errors logged at the call site if/when we wire
			// structured logging. A failed trim just means disk grows slightly.
			_ = b.trim()
		}
	}
}

func (b *sqliteBackend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.cancel()
		b.wg.Wait()
		err = b.db.Close()
	})
	return err
}
