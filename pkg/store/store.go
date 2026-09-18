// Package store defines epmon's persistence ports.
//
// Concrete implementations (SQLite today) live outside this package and
// satisfy the Store interface. Callers — scheduler, api, cmd — depend only
// on what follows, never on a driver. All timestamps are Unix epoch
// milliseconds; JSON rendering (RFC 3339) is the API layer's job.
//
// The command types below are public API covered by the v1 compatibility
// promise: epmon-cloud imports this package rather than forking it.
package store

import (
	"context"
	"errors"
	"time"
)

// Domain errors. Drivers must surface these, never driver types.
var (
	ErrNotFound    = errors.New("store: not found")
	ErrConflict    = errors.New("store: conflict")
	ErrUnavailable = errors.New("store: unavailable")
)

// Check is one recorded probe result. CheckedAt is Unix ms.
type Check struct {
	ID           int64  `json:"id"`
	ServiceID    string `json:"serviceId"`
	StatusCode   int    `json:"statusCode"`
	LatencyMs    int64  `json:"latencyMs"`
	Up           bool   `json:"up"`
	Reason       string `json:"reason,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	CheckedAt    int64  `json:"checkedAt"`
}

// CheckSample is the minimal projection the rollup scan needs.
type CheckSample struct {
	LatencyMs int64
	Up        bool
}

// Incident states and severities (see API state machine).
const (
	StateInvestigating = "investigating"
	StateIdentified    = "identified"
	StateMonitoring    = "monitoring"
	StateResolved      = "resolved"

	SeverityMinor    = "minor"
	SeverityMajor    = "major"
	SeverityCritical = "critical"

	SourceManual = "manual"
	SourceAuto   = "auto"
)

// StateOrder ranks states for forward-only transitions.
var StateOrder = map[string]int{
	StateInvestigating: 0,
	StateIdentified:    1,
	StateMonitoring:    2,
	StateResolved:      3,
}

// ValidState reports whether s names a known incident state.
func ValidState(s string) bool {
	_, ok := StateOrder[s]
	return ok
}

// ValidSeverity reports whether s names a known severity.
func ValidSeverity(s string) bool {
	return s == SeverityMinor || s == SeverityMajor || s == SeverityCritical
}

// Incident is a status entry with its update thread. ServiceID nil means
// platform-wide. ResolvedAt is 0 when unresolved.
type Incident struct {
	ID         int64            `json:"id"`
	ServiceID  *string          `json:"serviceId"`
	Title      string           `json:"title"`
	State      string           `json:"state"`
	Severity   string           `json:"severity"`
	Source     string           `json:"source"`
	CreatedAt  int64            `json:"createdAt"`
	ResolvedAt int64            `json:"resolvedAt"`
	Updates    []IncidentUpdate `json:"updates"`
}

// IncidentUpdate is one timestamped line on an incident.
type IncidentUpdate struct {
	ID         int64  `json:"id"`
	IncidentID int64  `json:"incidentId"`
	Text       string `json:"text"`
	CreatedAt  int64  `json:"createdAt"`
}

// DayRollup is one calendar-day bucket. UptimePct, AvgLatencyMs and
// P95LatencyMs are nil iff Total == 0 (honest null, never fabricated).
type DayRollup struct {
	ServiceID    string   `json:"serviceId"`
	Date         string   `json:"date"`
	Total        int      `json:"total"`
	Failures     int      `json:"failures"`
	UptimePct    *float64 `json:"uptimePct"`
	AvgLatencyMs *int64   `json:"avgLatencyMs"`
	P95LatencyMs *int64   `json:"p95LatencyMs"`
	ComputedAt   int64    `json:"computedAt"`
}

// ServiceSnapshot is the persisted service row: normalized config fields.
// ExpectStatus holds the normalized JSON integer array (§4.3.1); Headers
// holds the substituted JSON map.
type ServiceSnapshot struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	URL              string `json:"url"`
	Method           string `json:"method"`
	IntervalSeconds  int64  `json:"intervalSeconds"`
	TimeoutSeconds   int64  `json:"timeoutSeconds"`
	ExpectStatus     string `json:"expectStatus"`
	Headers          string `json:"headers"`
	BodyContains     string `json:"bodyContains"`
	FailureThreshold int    `json:"failureThreshold"`
	Archived         bool   `json:"archived"`
	CreatedAt        int64  `json:"createdAt"`
}

// incident creation / mutation commands (actor command types).

// OpenIncidentParams opens an incident; Body becomes the initial update.
type OpenIncidentParams struct {
	ServiceID *string
	Title     string
	Severity  string
	State     string
	Source    string
	Body      string
	Now       int64
}

// PatchIncidentParams mutates title/severity/state. Nil fields keep values.
type PatchIncidentParams struct {
	ID       int64
	Title    *string
	Severity *string
	State    *string
	Now      int64
}

// AppendUpdateParams appends one narrative line.
type AppendUpdateParams struct {
	IncidentID int64
	Text       string
	Now        int64
}

// MigrateServiceOp re-owns archived history from one id to another.
// It is only valid inside a ConfigDiff (never standalone).
type MigrateServiceOp struct {
	From string
	To   string
}

// ConfigDiff is the compound reload command: upserts + archives +
// alias migrations execute in one transaction, atomically.
type ConfigDiff struct {
	Upserts    []ServiceSnapshot
	Archives   []string
	Migrations []MigrateServiceOp
}

// MigratedPair records one completed history migration.
type MigratedPair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DiffSummary describes what ApplyConfigDiff did.
type DiffSummary struct {
	Added     []string       `json:"added"`
	Updated   []string       `json:"updated"`
	Archived  []string       `json:"archived"`
	Migrated  []MigratedPair `json:"migrated"`
	Unchanged int            `json:"unchanged"`
	NoMatch   []string       `json:"noMatch"`
}

// IncidentFilter narrows ListIncidents. Page is 1-based.
type IncidentFilter struct {
	ServiceID string
	State     string
	Open      *bool
	Page      int
	PerPage   int
}

// Store is the actor interface: the sole mutation path plus reads.
// Writes (except SubmitCheck) block until the actor executes them or ctx
// expires, in which case they return ErrUnavailable. SubmitCheck never
// blocks: on overflow it drops with a counter and an error log.
//
// Cross-channel ordering caveat: a check (checkCh) and its derived
// auto-incident command (cmdCh) travel different channels, so an incident
// row MAY become visible before its triggering check. Checks always land
// eventually; incident createdAt is authoritative.
//
// Re-entrancy prohibition: callers MUST NOT submit a command while holding
// a resource another command awaits (no nested submits).
type Store interface {
	// SubmitCheck enqueues one probe result. Never blocks; drops with a
	// counter on overflow.
	SubmitCheck(c Check)
	// OpenIncident inserts an incident plus its initial update.
	OpenIncident(ctx context.Context, p OpenIncidentParams) (*Incident, error)
	// PatchIncident applies the forward-only state machine.
	PatchIncident(ctx context.Context, p PatchIncidentParams) (*Incident, error)
	// AppendUpdate appends one update line, returning it.
	AppendUpdate(ctx context.Context, p AppendUpdateParams) (*IncidentUpdate, error)
	// ApplyConfigDiff executes the compound reload transaction.
	ApplyConfigDiff(ctx context.Context, d ConfigDiff) (*DiffSummary, error)
	// RecomputeRollups compacts complete day buckets (idempotent).
	RecomputeRollups(ctx context.Context, timezone string, now time.Time) error
	// PruneRetention deletes raw checks and rollups past retention.
	PruneRetention(ctx context.Context, checksDays, rollupsDays int, now time.Time) error

	// Ping verifies the database answers (read pool).
	Ping(ctx context.Context) error
	// LastCheck returns the newest check, or nil, nil when none exists.
	LastCheck(ctx context.Context, serviceID string) (*Check, error)
	// RecentUpTail returns up to limit newest up-flags, newest first.
	RecentUpTail(ctx context.Context, serviceID string, limit int) ([]bool, error)
	// ChecksInRange scans one day bucket: SELECT latency_ms, up WHERE
	// service_id=? AND checked_at>=? AND checked_at<?.
	ChecksInRange(ctx context.Context, serviceID string, fromMs, toMs int64) ([]CheckSample, error)
	// ListServices returns service rows (archived included iff asked).
	ListServices(ctx context.Context, includeArchived bool) ([]ServiceSnapshot, error)
	// GetService returns one service row, or nil, nil.
	GetService(ctx context.Context, id string) (*ServiceSnapshot, error)
	// RollupsFor returns persisted buckets keyed by date.
	RollupsFor(ctx context.Context, serviceID string, dates []string) (map[string]*DayRollup, error)
	// GetIncident returns one incident with its thread, or nil, nil.
	GetIncident(ctx context.Context, id int64) (*Incident, error)
	// ListIncidents returns newest-first incidents plus the total count.
	ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, int, error)
	// OpenIncidentForService returns the open incident for a service, if any.
	OpenIncidentForService(ctx context.Context, serviceID string) (*Incident, error)
	// MergedTo reports an alias-migration target for an archived id.
	MergedTo(old string) (string, bool)

	// QueueDepths returns pending (cmd, check) counts for metrics.
	QueueDepths() (cmd, check int)
	// DroppedTotal counts checkCh overflow drops.
	DroppedTotal() uint64
	// WriteTimeoutTotal counts command-budget expiries.
	WriteTimeoutTotal() uint64

	// Close flushes cmdCh and checkCh fully, resolves every pending
	// resultCh, then closes the database.
	Close() error
}

// MsToTime converts Unix ms to UTC time.
func MsToTime(ms int64) time.Time {
	return time.Unix(ms/1000, (ms%1000)*1e6).UTC()
}

// TimeToMs converts a time to Unix ms.
func TimeToMs(t time.Time) int64 {
	return t.Unix()*1000 + int64(t.Nanosecond())/1e6
}

// NowMs returns the current Unix ms.
func NowMs() int64 { return TimeToMs(time.Now()) }
