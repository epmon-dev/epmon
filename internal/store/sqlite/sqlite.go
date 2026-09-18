// Package sqlite is the SQLite adapter for the store ports.
//
// It owns every SQL statement in the codebase — swapping databases means
// adding a sibling package (e.g. internal/store/postgres), not touching
// callers. Timestamps are unix seconds (UTC). Daily buckets derive on read,
// so retention is a single DELETE with no rollup jobs.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/epmon-dev/epmon/internal/store"
	_ "modernc.org/sqlite"
)

// Compile-time proof the adapter satisfies the ports.
var _ store.Store = (*Store)(nil)

func init() {
	// Self-registration: importing this package (even blank) makes the
	// "sqlite" driver available to store.Open. No caller names this type.
	store.Register("sqlite", func(_ context.Context, dsn string) (store.Store, error) {
		return Open(dsn)
	})
}

const schema = `
CREATE TABLE IF NOT EXISTS services(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS checks(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  up INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_checks_service_ts ON checks(service_id, ts);
CREATE TABLE IF NOT EXISTS incidents(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  severity TEXT NOT NULL DEFAULT 'minor',
  state TEXT NOT NULL DEFAULT 'investigating',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS incident_updates(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  ts INTEGER NOT NULL,
  text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_updates_incident ON incident_updates(incident_id, ts);
`

// dayDownRatio: a day counts as down only when at least 10% of its probes
// failed (flap tolerance).
const dayDownRatio = 0.1

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
}

// Open creates the file if needed and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// SyncServices registers the configured catalogue (idempotent).
func (s *Store) SyncServices(ctx context.Context, services []store.ServiceMeta, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, svc := range services {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO services(id, name, url, updated_at) VALUES(?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name, url=excluded.url, updated_at=excluded.updated_at`,
			svc.ID, svc.Name, svc.URL, now.Unix(),
		); err != nil {
			return fmt.Errorf("sync service %q: %w", svc.ID, err)
		}
	}
	return tx.Commit()
}

// RecordCheck appends one probe result.
func (s *Store) RecordCheck(ctx context.Context, c store.Check) error {
	up := 0
	if c.Up {
		up = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO checks(service_id, ts, up, latency_ms, status_code, error)
		 VALUES(?,?,?,?,?,?)`,
		c.ServiceID, c.TS, up, c.LatencyMs, c.StatusCode, c.Error,
	)
	return err
}

func scanCheck(row interface{ Scan(...any) error }) (store.Check, error) {
	var c store.Check
	var up int
	if err := row.Scan(&c.ID, &c.ServiceID, &c.TS, &up, &c.LatencyMs, &c.StatusCode, &c.Error); err != nil {
		return store.Check{}, err
	}
	c.Up = up == 1
	return c, nil
}

// LastCheck returns the newest recorded check, or nil, nil if none exists yet.
func (s *Store) LastCheck(ctx context.Context, serviceID string) (*store.Check, error) {
	c, err := scanCheck(s.db.QueryRowContext(ctx,
		`SELECT id, service_id, ts, up, latency_ms, status_code, error
		 FROM checks WHERE service_id=? ORDER BY ts DESC, id DESC LIMIT 1`,
		serviceID,
	))
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// RecentChecks returns up to limit newest checks, newest first.
func (s *Store) RecentChecks(ctx context.Context, serviceID string, limit int) ([]store.Check, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service_id, ts, up, latency_ms, status_code, error
		 FROM checks WHERE service_id=? ORDER BY ts DESC, id DESC LIMIT ?`,
		serviceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Check{}
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DailyHistory returns days oldest-first, nil Up for days without probes.
func (s *Store) DailyHistory(ctx context.Context, serviceID string, days int, now time.Time) ([]store.DayBucket, error) {
	byDay := map[string]struct {
		up, n int
	}{}
	cutoff := now.AddDate(0, 0, -(days - 1))
	rows, err := s.db.QueryContext(ctx,
		`SELECT CAST(ts/86400 AS INTEGER) AS day, SUM(up), COUNT(*)
		 FROM checks WHERE service_id=? AND ts>=? GROUP BY day`,
		serviceID, cutoff.Unix(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var day int64
		var a struct {
			up, n int
		}
		if err := rows.Scan(&day, &a.up, &a.n); err != nil {
			return nil, err
		}
		date := time.Unix(day*86400, 0).UTC().Format("2006-01-02")
		byDay[date] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]store.DayBucket, 0, days)
	for i := days - 1; i >= 0; i-- {
		date := now.AddDate(0, 0, -i).UTC().Format("2006-01-02")
		b := store.DayBucket{Date: date}
		if a, ok := byDay[date]; ok && a.n > 0 {
			up := float64(a.n-a.up)/float64(a.n) < dayDownRatio
			b.Up = &up
			b.Checks = a.n
		}
		out = append(out, b)
	}
	return out, nil
}

// Purge deletes checks older than retentionDays. Returns rows removed.
func (s *Store) Purge(ctx context.Context, retentionDays int, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM checks WHERE ts < ?`,
		now.AddDate(0, 0, -retentionDays).Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CreateIncident opens a manual incident. Severity normalizes to minor
// unless "major".
func (s *Store) CreateIncident(ctx context.Context, serviceID, title, severity string, now time.Time) (int64, error) {
	if severity != "major" {
		severity = "minor"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents(service_id, title, severity, state, created_at, updated_at)
		 VALUES(?,?,?,?,?,?)`,
		serviceID, title, severity, "investigating", now.Unix(), now.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateIncident mutates title/severity/state; empty args keep the field.
func (s *Store) UpdateIncident(ctx context.Context, id int64, title, severity, state string, now time.Time) error {
	cur, err := s.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	if cur == nil {
		return store.ErrNotFound
	}
	if title != "" {
		cur.Title = title
	}
	if severity == "minor" || severity == "major" {
		cur.Severity = severity
	}
	switch state {
	case "investigating", "monitoring", "resolved":
		cur.State = state
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE incidents SET title=?, severity=?, state=?, updated_at=? WHERE id=?`,
		cur.Title, cur.Severity, cur.State, now.Unix(), id,
	)
	return err
}

// AddIncidentUpdate appends one timestamped line to an incident's thread.
func (s *Store) AddIncidentUpdate(ctx context.Context, id int64, text string, now time.Time) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM incidents WHERE id=?`, id).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return store.ErrNotFound
		}
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO incident_updates(incident_id, ts, text) VALUES(?,?,?)`,
		id, now.Unix(), text,
	); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE incidents SET updated_at=? WHERE id=?`, now.Unix(), id)
	return err
}

// GetIncident returns one incident with its updates, or nil, nil.
func (s *Store) GetIncident(ctx context.Context, id int64) (*store.Incident, error) {
	incidents, err := s.listIncidents(ctx, `WHERE i.id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(incidents) == 0 {
		return nil, nil
	}
	return &incidents[0], nil
}

// ListIncidents returns newest-first incidents matching the filter.
func (s *Store) ListIncidents(ctx context.Context, f store.IncidentFilter) ([]store.Incident, error) {
	conds := []string{}
	args := []any{}
	if f.State != "" {
		conds = append(conds, "i.state=?")
		args = append(args, f.State)
	}
	if f.ServiceID != "" {
		conds = append(conds, "i.service_id=?")
		args = append(args, f.ServiceID)
	}
	filter := `ORDER BY i.created_at DESC, i.id DESC`
	if len(conds) > 0 {
		filter = "WHERE " + strings.Join(conds, " AND ") + " " + filter
	}
	return s.listIncidents(ctx, filter, args...)
}

func (s *Store) listIncidents(ctx context.Context, filter string, args ...any) ([]store.Incident, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT i.id, i.service_id, i.title, i.severity, i.state, i.created_at, i.updated_at
		 FROM incidents i `+filter,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Incident{}
	for rows.Next() {
		var in store.Incident
		if err := rows.Scan(&in.ID, &in.ServiceID, &in.Title, &in.Severity, &in.State, &in.CreatedAt, &in.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		updates, err := s.incidentUpdates(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Updates = updates
	}
	return out, nil
}

func (s *Store) incidentUpdates(ctx context.Context, incidentID int64) ([]store.Update, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, text FROM incident_updates WHERE incident_id=? ORDER BY ts ASC, id ASC`,
		incidentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	updates := []store.Update{}
	for rows.Next() {
		var u store.Update
		if err := rows.Scan(&u.TS, &u.Text); err != nil {
			return nil, err
		}
		updates = append(updates, u)
	}
	return updates, rows.Err()
}
