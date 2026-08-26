// Package telemetry records usage stats with an in-memory buffer flushed to
// SQLite on an interval. The hot path never touches disk: Record only appends
// to a slice under a mutex; a background goroutine drains the buffer in one
// transaction. A crash may lose events from the current interval.
package telemetry

import (
	"database/sql"
	"encoding/json"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Event struct {
	Provider  string
	Chain     string
	Model     string
	Stream    bool
	Status    int
	TokensIn  int64
	TokensOut int64
	Key       string
	Err       string
	Ts        time.Time
}

type Telemetry struct {
	mu     sync.Mutex
	buffer []Event
	db     *sql.DB
	stop   chan struct{}
	wg     sync.WaitGroup
}

// Open creates the database (if needed), applies the schema, and starts the
// flusher goroutine. interval <= 0 disables background flushing; Close still
// flushes once.
func Open(path string, interval time.Duration) (*Telemetry, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL,
		provider TEXT NOT NULL,
		chain TEXT NOT NULL,
		model TEXT NOT NULL,
		stream INTEGER NOT NULL,
		status INTEGER NOT NULL,
		tokens_in INTEGER NOT NULL DEFAULT 0,
		tokens_out INTEGER NOT NULL DEFAULT 0,
		key TEXT NOT NULL DEFAULT '',
		err TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	t := &Telemetry{db: db, stop: make(chan struct{})}
	if interval > 0 {
		t.wg.Add(1)
		go t.flusher(interval)
	}
	return t, nil
}

// migrate adds columns introduced after the initial schema to existing
// databases (e.g. an existing production volume) without dropping data.
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !cols["key"] {
		if _, err := db.Exec(`ALTER TABLE requests ADD COLUMN key TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

func (t *Telemetry) Record(e Event) {
	t.mu.Lock()
	t.buffer = append(t.buffer, e)
	t.mu.Unlock()
}

func (t *Telemetry) flusher(interval time.Duration) {
	defer t.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := t.Flush(); err != nil {
				log.Printf("telemetry flush: %v", err)
			}
		case <-t.stop:
			return
		}
	}
}

// Flush drains the buffer into a single transaction.
func (t *Telemetry) Flush() error {
	t.mu.Lock()
	events := t.buffer
	t.buffer = nil
	t.mu.Unlock()
	if len(events) == 0 {
		return nil
	}
	tx, err := t.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO requests
		(ts, provider, chain, model, stream, status, tokens_in, tokens_out, key, err)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, e := range events {
		if _, err := stmt.Exec(e.Ts.UTC().Format(time.RFC3339), e.Provider, e.Chain,
			e.Model, e.Stream, e.Status, e.TokensIn, e.TokensOut, e.Key, e.Err); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Close stops the flusher and performs a final flush.
func (t *Telemetry) Close() error {
	close(t.stop)
	t.wg.Wait()
	if err := t.Flush(); err != nil {
		return err
	}
	return t.db.Close()
}

// DB exposes the underlying handle for read-only inspection.
func (t *Telemetry) DB() *sql.DB { return t.db }

// Stats is an aggregate usage snapshot for the admin surface.
type Stats struct {
	Requests   int            `json:"requests"`
	Streams    int            `json:"streams"`
	Errors     int            `json:"errors"`
	TokensIn   int64          `json:"tokens_in"`
	TokensOut  int64          `json:"tokens_out"`
	ByProvider []ProviderStat `json:"by_provider"`
}

type ProviderStat struct {
	Provider  string `json:"provider"`
	Requests  int    `json:"requests"`
	Errors    int    `json:"errors"`
	TokensIn  int64  `json:"tokens_in"`
	TokensOut int64  `json:"tokens_out"`
}

// Stats aggregates persisted events. Data reflects the last flush (up to the
// flush interval old) — good enough for observability.
func (t *Telemetry) Stats() (Stats, error) {
	var s Stats
	err := t.db.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(stream),0),
		COALESCE(SUM(CASE WHEN status < 200 OR status >= 300 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(tokens_in),0),
		COALESCE(SUM(tokens_out),0)
		FROM requests`).Scan(&s.Requests, &s.Streams, &s.Errors, &s.TokensIn, &s.TokensOut)
	if err != nil {
		return s, err
	}
	rows, err := t.db.Query(`SELECT provider, COUNT(*),
		COALESCE(SUM(CASE WHEN status < 200 OR status >= 300 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		FROM requests GROUP BY provider ORDER BY provider`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var ps ProviderStat
		if err := rows.Scan(&ps.Provider, &ps.Requests, &ps.Errors, &ps.TokensIn, &ps.TokensOut); err != nil {
			return s, err
		}
		s.ByProvider = append(s.ByProvider, ps)
	}
	return s, rows.Err()
}

// usage is the minimal OpenAI usage object.
type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// ParseUsage extracts token counts from an OpenAI chat completion body.
// Returns 0,0 when absent — counting is best-effort.
func ParseUsage(body []byte) (in, out int64) {
	var resp struct {
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0
	}
	if resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0 {
		return resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}
	return 0, 0
}
