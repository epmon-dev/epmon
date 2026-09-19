// Package store defines epmon's persistence ports.
//
// Concrete adapters (SQLite today, Postgres tomorrow) live in subpackages
// and satisfy these interfaces. Callers — scheduler, api, cmd — depend only
// on what follows, never on a driver. Domain errors are exposed as
// ErrNotFound so no database/sql type ever leaks across the boundary.
package store

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// ErrNotFound is returned when an incident id names nothing.
var ErrNotFound = errors.New("store: not found")

// TransitionError reports a rejected backward incident state move,
// carrying the from→to pair for 409 responses.
type TransitionError struct {
	From string
	To   string
}

// Error implements error.
func (e *TransitionError) Error() string {
	return "invalid state transition from " + strconv.Quote(e.From) + " to " + strconv.Quote(e.To)
}

// ErrInvalid is returned when input fails domain validation
// (e.g. over-long incident update text).
var ErrInvalid = errors.New("store: invalid")

// MaxUpdateRunes bounds incident update text to 1..2000 runes.
const MaxUpdateRunes = 2000

// Check is one recorded probe result.
type Check struct {
	ID         int64  `json:"id"`
	ServiceID  string `json:"service_id"`
	TS         int64  `json:"ts"`
	Up         bool   `json:"up"`
	LatencyMs  int64  `json:"latency_ms"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
}

// DayBucket is one UTC day of aggregated probes.
// Up is nil on days without probes — never fabricated.
type DayBucket struct {
	Date   string `json:"date"`
	Up     *bool  `json:"up"`
	Checks int    `json:"checks"`
}

// Incident is a human-written status entry with its update thread.
type Incident struct {
	ID        int64    `json:"id"`
	ServiceID string   `json:"service_id,omitempty"`
	Title     string   `json:"title"`
	Severity  string   `json:"severity"`
	State     string   `json:"state"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
	Updates   []Update `json:"updates"`
}

// Update is one timestamped line on an incident.
type Update struct {
	TS   int64  `json:"ts"`
	Text string `json:"text"`
}

// ServiceMeta is the catalogue row mirrored from config.
type ServiceMeta struct {
	ID   string
	Name string
	URL  string
}

// CheckRecorder is the write side schedulers need.
type CheckRecorder interface {
	RecordCheck(ctx context.Context, c Check) error
	Purge(ctx context.Context, retentionDays int, now time.Time) (int64, error)
}

// CheckReader is the read side the API needs.
type CheckReader interface {
	// LastCheck returns nil, nil when nothing was recorded yet.
	LastCheck(ctx context.Context, serviceID string) (*Check, error)
	RecentChecks(ctx context.Context, serviceID string, limit int) ([]Check, error)
	// DailyHistory returns days oldest-first, nil Up for probeless days.
	DailyHistory(ctx context.Context, serviceID string, days int, now time.Time) ([]DayBucket, error)
}

// IncidentFilter narrows ListIncidents. Empty fields disable that filter.
// Limit caps the page (<=0 means unbounded for direct store users; the
// API always passes a validated bound). Offset skips that many newest-first
// rows.
type IncidentFilter struct {
	State     string // investigating|monitoring|resolved, "" = any
	ServiceID string // "" = any service
	Limit     int
	Offset    int
}

// IncidentStore is the manual incident log.
type IncidentStore interface {
	// CreateIncident opens an incident in "investigating" state.
	// Severity is normalized to minor unless "major".
	CreateIncident(ctx context.Context, serviceID, title, severity string, now time.Time) (int64, error)
	// GetIncident returns nil, nil for unknown ids.
	GetIncident(ctx context.Context, id int64) (*Incident, error)
	// ListIncidents returns newest-first incidents matching the filter.
	ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error)
	// CountIncidents returns the total matches for the filter's State and
	// ServiceID (ignoring Limit/Offset), for paged responses.
	CountIncidents(ctx context.Context, f IncidentFilter) (int, error)
	// UpdateIncident mutates title/severity/state; empty args keep the field.
	// State must be investigating|monitoring|resolved. Unknown id → ErrNotFound.
	UpdateIncident(ctx context.Context, id int64, title, severity, state string, now time.Time) error
	// AddIncidentUpdate appends to an incident's thread. Unknown id → ErrNotFound.
	AddIncidentUpdate(ctx context.Context, id int64, text string, now time.Time) error
}

// ServiceRegistry mirrors the configured catalogue into storage.
type ServiceRegistry interface {
	SyncServices(ctx context.Context, services []ServiceMeta, now time.Time) error
}

// Store composes every port. Adapters implement this and callers accept
// the narrow interface they actually use — see CheckRecorder.
type Store interface {
	CheckRecorder
	CheckReader
	IncidentStore
	ServiceRegistry
	// Ping verifies the database answers (readiness).
	Ping(ctx context.Context) error
	Close() error
}
