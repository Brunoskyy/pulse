// Package store keeps probe results and incidents in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, no cgo

	"github.com/Brunoskyy/pulse/internal/check"
)

// Store is safe for concurrent use. SQLite allows one writer at a time, so
// writes are serialized here with a mutex rather than left to fail with
// SQLITE_BUSY; reads run concurrently thanks to WAL mode.
type Store struct {
	db *sql.DB
	mu sync.Mutex // guards writes
}

// Incident is a period during which a check was considered down.
type Incident struct {
	ID        int64
	CheckID   string
	StartedAt time.Time
	EndedAt   *time.Time
	Reason    string
}

func (i Incident) Duration(now time.Time) time.Duration {
	end := now
	if i.EndedAt != nil {
		end = *i.EndedAt
	}
	return end.Sub(i.StartedAt)
}

const schema = `
CREATE TABLE IF NOT EXISTS results (
  check_id   TEXT    NOT NULL,
  at         INTEGER NOT NULL, -- unix milliseconds
  ok         INTEGER NOT NULL,
  latency_us INTEGER NOT NULL,
  error      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS results_check_at ON results(check_id, at);
CREATE TABLE IF NOT EXISTS incidents (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  check_id   TEXT    NOT NULL,
  started_at INTEGER NOT NULL,
  ended_at   INTEGER,
  reason     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS incidents_check ON incidents(check_id, started_at);
`

// Open opens (and creates) the database at path. ":memory:" works for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		// A shared in-memory database, so every pooled connection sees the same data.
		dsn = "file::memory:?cache=shared"
	} else {
		dsn = "file:" + path
	}
	dsn += sep(dsn) + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

func sep(dsn string) string {
	for _, r := range dsn {
		if r == '?' {
			return "&"
		}
	}
	return "?"
}

func (s *Store) Close() error { return s.db.Close() }

// AddResult records one probe.
func (s *Store) AddResult(ctx context.Context, r check.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO results (check_id, at, ok, latency_us, error) VALUES (?, ?, ?, ?, ?)`,
		r.CheckID, r.At.UnixMilli(), boolInt(r.OK), r.Latency.Microseconds(), r.Error)
	return err
}

// AddResults records many probes in one transaction; used by the demo backfill.
func (s *Store) AddResults(ctx context.Context, rs []check.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO results (check_id, at, ok, latency_us, error) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rs {
		if _, err := stmt.ExecContext(ctx, r.CheckID, r.At.UnixMilli(), boolInt(r.OK), r.Latency.Microseconds(), r.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OpenIncident inserts a new open incident and returns its id.
func (s *Store) OpenIncident(ctx context.Context, checkID string, startedAt time.Time, reason string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents (check_id, started_at, reason) VALUES (?, ?, ?)`, checkID, startedAt.UnixMilli(), reason)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseIncident sets the end of an open incident. Closing an already closed
// one is a no-op, so a retried close cannot move the end time.
func (s *Store) CloseIncident(ctx context.Context, id int64, endedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, endedAt.UnixMilli(), id)
	return err
}

// AddIncident inserts a closed incident; used by the demo backfill.
func (s *Store) AddIncident(ctx context.Context, in Incident) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ended any
	if in.EndedAt != nil {
		ended = in.EndedAt.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO incidents (check_id, started_at, ended_at, reason) VALUES (?, ?, ?, ?)`,
		in.CheckID, in.StartedAt.UnixMilli(), ended, in.Reason)
	return err
}

// OpenIncidents returns incidents without an end, keyed by check. Used on
// startup so a restart does not open a second incident for an outage that
// was already being tracked.
func (s *Store) OpenIncidents(ctx context.Context) (map[string]Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, check_id, started_at, reason FROM incidents WHERE ended_at IS NULL ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Incident{}
	for rows.Next() {
		var in Incident
		var started int64
		if err := rows.Scan(&in.ID, &in.CheckID, &started, &in.Reason); err != nil {
			return nil, err
		}
		in.StartedAt = time.UnixMilli(started)
		out[in.CheckID] = in
	}
	return out, rows.Err()
}

// Incidents returns incidents that overlap [since, now], newest first.
func (s *Store) Incidents(ctx context.Context, since time.Time, limit int) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, check_id, started_at, ended_at, reason FROM incidents
		 WHERE ended_at IS NULL OR ended_at >= ?
		 ORDER BY started_at DESC LIMIT ?`, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var in Incident
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&in.ID, &in.CheckID, &started, &ended, &in.Reason); err != nil {
			return nil, err
		}
		in.StartedAt = time.UnixMilli(started)
		if ended.Valid {
			t := time.UnixMilli(ended.Int64)
			in.EndedAt = &t
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// Window summarises a check over [from, to).
type Window struct {
	Total   int
	OK      int
	P95     time.Duration
	HasData bool
}

// Uptime is the share of probes that succeeded, or -1 when nothing was
// observed. Periods when Pulse itself was not running are not counted as
// downtime: no probe, no verdict.
func (w Window) Uptime() float64 {
	if w.Total == 0 {
		return -1
	}
	return float64(w.OK) / float64(w.Total)
}

// Summary computes the uptime and p95 latency of successful probes in a window.
func (s *Store) Summary(ctx context.Context, checkID string, from, to time.Time) (Window, error) {
	var w Window
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(ok), 0) FROM results WHERE check_id = ? AND at >= ? AND at < ?`,
		checkID, from.UnixMilli(), to.UnixMilli()).Scan(&w.Total, &w.OK)
	if err != nil {
		return w, err
	}
	w.HasData = w.Total > 0
	if w.OK == 0 {
		return w, nil
	}
	// Nearest-rank p95 over successful probes only: a timeout is already
	// counted as downtime, and letting it into the latency figure would count
	// it twice.
	rank := percentileRank(w.OK, 0.95)
	var us int64
	err = s.db.QueryRowContext(ctx,
		`SELECT latency_us FROM results WHERE check_id = ? AND at >= ? AND at < ? AND ok = 1
		 ORDER BY latency_us LIMIT 1 OFFSET ?`,
		checkID, from.UnixMilli(), to.UnixMilli(), rank-1).Scan(&us)
	if err != nil {
		return w, err
	}
	w.P95 = time.Duration(us) * time.Microsecond
	return w, nil
}

// percentileRank is the 1-based nearest-rank index for percentile p of n values.
func percentileRank(n int, p float64) int {
	r := int(p*float64(n) + 0.999999)
	if r < 1 {
		r = 1
	}
	if r > n {
		r = n
	}
	return r
}

// Day is one cell of the 90-day strip.
type Day struct {
	Start time.Time
	Total int
	OK    int
}

// Days returns one entry per UTC day from the day containing from up to and
// including the day containing to, oldest first. Days with no probes are
// present with Total 0, so the strip has no holes.
func (s *Store) Days(ctx context.Context, checkID string, from, to time.Time) ([]Day, error) {
	first := from.UTC().Truncate(24 * time.Hour)
	last := to.UTC().Truncate(24 * time.Hour)
	n := int(last.Sub(first)/(24*time.Hour)) + 1
	days := make([]Day, n)
	for i := range days {
		days[i].Start = first.Add(time.Duration(i) * 24 * time.Hour)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT at / 86400000 AS day, COUNT(*), SUM(ok) FROM results
		 WHERE check_id = ? AND at >= ? AND at < ?
		 GROUP BY day`, checkID, first.UnixMilli(), last.Add(24*time.Hour).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	base := first.UnixMilli() / 86400000
	for rows.Next() {
		var day int64
		var d Day
		if err := rows.Scan(&day, &d.Total, &d.OK); err != nil {
			return nil, err
		}
		if i := int(day - base); i >= 0 && i < n {
			days[i].Total, days[i].OK = d.Total, d.OK
		}
	}
	return days, rows.Err()
}

// Last returns the most recent result for a check.
func (s *Store) Last(ctx context.Context, checkID string) (check.Result, bool, error) {
	var r check.Result
	var at, us int64
	var ok int
	err := s.db.QueryRowContext(ctx,
		`SELECT at, ok, latency_us, error FROM results WHERE check_id = ? ORDER BY at DESC LIMIT 1`, checkID).
		Scan(&at, &ok, &us, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r.CheckID, r.At, r.OK, r.Latency = checkID, time.UnixMilli(at), ok == 1, time.Duration(us)*time.Microsecond
	return r, true, nil
}

// Recent returns the last n results for a check, oldest first.
func (s *Store) Recent(ctx context.Context, checkID string, n int) ([]check.Result, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, ok, latency_us, error FROM results WHERE check_id = ? ORDER BY at DESC LIMIT ?`, checkID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []check.Result
	for rows.Next() {
		var r check.Result
		var at, us int64
		var ok int
		if err := rows.Scan(&at, &ok, &us, &r.Error); err != nil {
			return nil, err
		}
		r.CheckID, r.At, r.OK, r.Latency = checkID, time.UnixMilli(at), ok == 1, time.Duration(us)*time.Microsecond
		out = append(out, r)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// PruneBatch is how many rows one delete removes. Pruning 90 days of a busy
// config in one statement would hold the write lock long enough to delay
// fresh results; batches let them in between.
const PruneBatch = 5000

// Prune deletes results and closed incidents older than cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		s.mu.Lock()
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM results WHERE rowid IN (SELECT rowid FROM results WHERE at < ? LIMIT ?)`, cutoff.UnixMilli(), PruneBatch)
		s.mu.Unlock()
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < PruneBatch {
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM incidents WHERE ended_at IS NOT NULL AND ended_at < ?`, cutoff.UnixMilli())
	return total, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
