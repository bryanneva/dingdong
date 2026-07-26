package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
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

CREATE TABLE IF NOT EXISTS webhook_subscribers (
    id            TEXT PRIMARY KEY,
    url           TEXT NOT NULL,
    topic         TEXT NOT NULL DEFAULT '',
    secret        TEXT NOT NULL,
    created_at_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id            TEXT PRIMARY KEY,
    subscriber_id TEXT NOT NULL,
    knock_json    TEXT NOT NULL,
    attempt       INTEGER NOT NULL DEFAULT 0,
    next_at_ns    INTEGER NOT NULL,
    last_status   INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX IF NOT EXISTS idx_deliveries_next_at ON webhook_deliveries(next_at_ns);
`

// trimInterval is the cadence at which the retention loop fires. Hourly is
// plenty for a service with at most ~100 knocks/min peak.
const trimInterval = 1 * time.Hour

// NewSQLiteStore opens (or creates) a SQLite database at path and returns
// a Store wrapping it.
func NewSQLiteStore(path string, retentionRows int) (*Store, error) {
	b, err := newSQLiteBackend(path, retentionRows)
	if err != nil {
		return nil, err
	}
	return NewStore(b), nil
}

func newSQLiteBackend(path string, retentionRows int) (*sqliteBackend, error) {
	if retentionRows <= 0 {
		retentionRows = 100000
	}
	// WAL + synchronous=NORMAL is the standard tradeoff for a single-pod
	// service: crash-safe (durable on fsync at checkpoint), no per-commit
	// fsync cost. busy_timeout backs off briefly on lock contention.
	// Escape the path so a stray `?` or `&` from a test-tempdir layout can't
	// shadow the pragma query string and silently disable WAL.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		url.PathEscape(path),
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

// --- WebhookBackend implementation ---------------------------------------

func (b *sqliteBackend) SaveWebhook(w Webhook) error {
	_, err := b.db.Exec(
		`INSERT INTO webhook_subscribers (id, url, topic, secret, created_at_ns) VALUES (?, ?, ?, ?, ?)`,
		w.ID, w.URL, w.Topic, w.Secret, w.CreatedAt.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("save webhook: %w", err)
	}
	return nil
}

func (b *sqliteBackend) LoadWebhooks() ([]Webhook, error) {
	rows, err := b.db.Query(
		`SELECT id, url, topic, secret, created_at_ns FROM webhook_subscribers ORDER BY created_at_ns ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("load webhooks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Webhook
	for rows.Next() {
		var w Webhook
		var tsNs int64
		if err := rows.Scan(&w.ID, &w.URL, &w.Topic, &w.Secret, &tsNs); err != nil {
			return nil, fmt.Errorf("load webhooks scan: %w", err)
		}
		w.CreatedAt = time.Unix(0, tsNs).UTC()
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load webhooks rows: %w", err)
	}
	return out, nil
}

func (b *sqliteBackend) RemoveWebhook(id string) error {
	_, err := b.db.Exec(`DELETE FROM webhook_subscribers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove webhook: %w", err)
	}
	return nil
}

func (b *sqliteBackend) EnqueueDelivery(d PendingDelivery) error {
	_, err := b.db.Exec(
		`INSERT INTO webhook_deliveries (id, subscriber_id, knock_json, attempt, next_at_ns, last_status, last_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Subscriber.ID, d.KnockJSON, d.Attempt, d.NextAt.UnixNano(), d.LastStatus, d.LastError,
	)
	if err != nil {
		return fmt.Errorf("enqueue delivery: %w", err)
	}
	return nil
}

func (b *sqliteBackend) FetchDueDeliveries(now time.Time, limit int) ([]PendingDelivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := b.db.Query(`
		SELECT d.id, d.knock_json, d.attempt, d.next_at_ns, d.last_status, d.last_error,
		       s.id, s.url, s.topic, s.secret, s.created_at_ns
		FROM webhook_deliveries d
		INNER JOIN webhook_subscribers s ON d.subscriber_id = s.id
		WHERE d.next_at_ns <= ?
		LIMIT ?`,
		now.UnixNano(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fetch due deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingDelivery
	for rows.Next() {
		var d PendingDelivery
		var nextNs, subCreatedNs int64
		if err := rows.Scan(
			&d.ID, &d.KnockJSON, &d.Attempt, &nextNs, &d.LastStatus, &d.LastError,
			&d.Subscriber.ID, &d.Subscriber.URL, &d.Subscriber.Topic, &d.Subscriber.Secret, &subCreatedNs,
		); err != nil {
			return nil, fmt.Errorf("fetch due deliveries scan: %w", err)
		}
		d.NextAt = time.Unix(0, nextNs).UTC()
		d.Subscriber.CreatedAt = time.Unix(0, subCreatedNs).UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fetch due deliveries rows: %w", err)
	}
	return out, nil
}

func (b *sqliteBackend) UpdateDelivery(id string, attempt int, nextAt time.Time, status int, errMsg string) error {
	_, err := b.db.Exec(
		`UPDATE webhook_deliveries SET attempt = ?, next_at_ns = ?, last_status = ?, last_error = ? WHERE id = ?`,
		attempt, nextAt.UnixNano(), status, errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("update delivery: %w", err)
	}
	return nil
}

func (b *sqliteBackend) DeleteDelivery(id string) error {
	_, err := b.db.Exec(`DELETE FROM webhook_deliveries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete delivery: %w", err)
	}
	return nil
}

func (b *sqliteBackend) CancelDeliveriesForWebhook(subscriberID string) error {
	_, err := b.db.Exec(`DELETE FROM webhook_deliveries WHERE subscriber_id = ?`, subscriberID)
	if err != nil {
		return fmt.Errorf("cancel deliveries: %w", err)
	}
	return nil
}
