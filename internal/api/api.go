// Package api serves epmon's versioned JSON API:
//
//	GET  /healthz (unversioned liveness probe, {"ok": true})
//	GET  /api/v1/status
//	GET  /api/v1/services
//	GET  /api/v1/services/{id}/history?days=90&limit=100
//	GET  /api/v1/incidents[?state=][?service=]
//	POST /api/v1/incidents
//	GET  /api/v1/incidents/{id}
//	PATCH /api/v1/incidents/{id}
//	POST /api/v1/incidents/{id}/updates
//	GET  /api/v1/openapi.yaml (this contract, embedded at build time)
//	GET  /api/v1/openapi.json (same contract as JSON)
//	GET  /docs (interactive reference UI)
//
// Conventions: single resources are bare objects; collections are
// {"<name>": [...], "total": n}; every error — including router 404s — is
// {"error": {"code": "<bad_request|not_found|internal>", "message": "…"}}.
// Trailing slashes are tolerated. 201 responses carry a Location header.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/store"
	"gopkg.in/yaml.v3"
)

// prefix versions every route. The next breaking change ships as /api/v2
// alongside it instead of moving these handlers.
const prefix = "/api/v1"

// openAPIYAML is the contract in openapi.yaml, embedded so the binary always
// serves the spec of the version answering — no drift, no extra files.
//
//go:embed openapi.yaml
var openAPIYAML []byte

// Server bundles config and storage for the handlers. Storage is the
// store.Store port — any adapter (SQLite, Postgres, …) plugs in here.
type Server struct {
	cfg   *config.Config
	store store.Store
	now   func() time.Time
}

// New builds a Server. now is injectable for tests (nil = time.Now).
func New(cfg *config.Config, st store.Store, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{cfg: cfg, store: st, now: now}
}

// Handler wires every route behind the production chain:
// recover → security headers → slash trim → CORS → rate limit →
// write auth → body cap → routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET "+prefix+"/status", s.status)
	mux.HandleFunc("GET "+prefix+"/services", s.services)
	mux.HandleFunc("GET "+prefix+"/services/{id}/history", s.history)
	mux.HandleFunc("GET "+prefix+"/incidents", s.listIncidents)
	mux.HandleFunc("POST "+prefix+"/incidents", s.createIncident)
	mux.HandleFunc("GET "+prefix+"/incidents/{id}", s.getIncident)
	mux.HandleFunc("PATCH "+prefix+"/incidents/{id}", s.updateIncident)
	mux.HandleFunc("POST "+prefix+"/incidents/{id}/updates", s.addUpdate)
	mux.HandleFunc("GET "+prefix+"/openapi.yaml", s.serveSpecYAML)
	mux.HandleFunc("GET "+prefix+"/openapi.json", s.serveSpecJSON)
	mux.HandleFunc("GET /docs", s.serveDocs)
	mux.HandleFunc("/", notFound)

	limiter := newRateLimiter(s.cfg.Server.RateLimitRPM, s.cfg.Server.RateLimitBurst)
	var h http.Handler = mux
	h = s.limitBody(h)
	h = s.requireWriteAuth(h)
	h = s.limitByIP(limiter, h)
	h = cors(s.cfg.Server.CORSAllowedOrigins, h)
	h = trimTrailingSlash(h)
	h = securityHeaders(h)
	h = recoverer(h)
	return h
}

// trimTrailingSlash makes /api/v1/services/ behave like /api/v1/services.
func trimTrailingSlash(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) > 1 {
			r.URL.Path = strings.TrimSuffix(r.URL.Path, "/")
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, httpCode int, code, msg string) {
	writeJSON(w, httpCode, map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusNotFound, "not_found", "unknown endpoint")
}

func (s *Server) serveSpecYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPIYAML)
}

func (s *Server) serveSpecJSON(w http.ResponseWriter, _ *http.Request) {
	var doc any
	if err := yaml.Unmarshal(openAPIYAML, &doc); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "spec conversion failed")
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// docsPage is a Scalar API reference pointed at the embedded spec.
// Only the UI shell loads from CDN; the contract itself is served locally.
const docsPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>epmon API Docs</title>
</head>
<body>
<script id="api-reference" data-url="/api/v1/openapi.yaml"></script>
<script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
<noscript><p>API reference needs JavaScript. Raw contract:
<a href="/api/v1/openapi.yaml">openapi.yaml</a> or
<a href="/api/v1/openapi.json">openapi.json</a>.</p></noscript>
</body>
</html>
`

func (s *Server) serveDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsPage))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	// Readiness, not just liveness: orchestration must stop routing here
	// when the database is gone.
	if err := s.store.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "store unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeDecodeErr maps body-decode failures: oversized payloads are 413,
// everything else 400.
func writeDecodeErr(w http.ResponseWriter, err error) {
	if isTooLarge(err) {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "body exceeds server limit")
		return
	}
	writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
}

type serviceState struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Up         *bool  `json:"up"`
	LatencyMs  *int64 `json:"latency_ms"`
	StatusCode *int   `json:"status_code"`
	CheckedAt  *int64 `json:"checked_at"`
	Error      string `json:"error,omitempty"`
}

func (s *Server) serviceStates(ctx context.Context) ([]serviceState, error) {
	states := make([]serviceState, 0, len(s.cfg.Services))
	for _, svc := range s.cfg.Services {
		st := serviceState{ID: svc.ID, Name: svc.Name, URL: svc.URL}
		last, err := s.store.LastCheck(ctx, svc.ID)
		if err != nil {
			return nil, err
		}
		if last != nil {
			up := last.Up
			st.Up = &up
			st.LatencyMs = &last.LatencyMs
			st.StatusCode = &last.StatusCode
			st.CheckedAt = &last.TS
			st.Error = last.Error
		}
		states = append(states, st)
	}
	return states, nil
}

func overallOf(states []serviceState) string {
	down, unknown := 0, 0
	for _, st := range states {
		switch {
		case st.Up == nil:
			unknown++
		case !*st.Up:
			down++
		}
	}
	switch {
	case down > 0:
		return "partial_outage"
	case unknown == len(states):
		return "unknown"
	default:
		return "operational"
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	states, err := s.serviceStates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checked_at": s.now().Unix(),
		"overall":    overallOf(states),
		"services":   states,
	})
}

func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	states, err := s.serviceStates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"services": states,
		"total":    len(states),
	})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	known := false
	for _, svc := range s.cfg.Services {
		if svc.ID == id {
			known = true
			break
		}
	}
	if !known {
		writeErr(w, http.StatusNotFound, "not_found", "unknown service")
		return
	}
	q := r.URL.Query()
	days, ok := intParam(q.Get("days"), 1, 365, 90)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "days must be 1..365")
		return
	}
	limit, ok := intParam(q.Get("limit"), 1, 500, 100)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "limit must be 1..500")
		return
	}
	buckets, err := s.store.DailyHistory(r.Context(), id, days, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	checks, err := s.store.RecentChecks(r.Context(), id, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	knownDays, upDays := 0, 0
	for _, b := range buckets {
		if b.Up != nil {
			knownDays++
			if *b.Up {
				upDays++
			}
		}
	}
	var uptime *float64
	if knownDays > 0 {
		v := float64(upDays) / float64(knownDays) * 100
		uptime = &v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service_id": id,
		"days":       days,
		"uptime_pct": uptime,
		"history":    buckets,
		"recent":     checks,
		"total":      len(checks),
	})
}

// intParam parses an optional query value with bounds, falling back to def
// when empty. ok=false on unparseable or out-of-range input.
func intParam(raw string, min, max, def int) (v int, ok bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	incidents, err := s.store.ListIncidents(r.Context(), store.IncidentFilter{
		State:     q.Get("state"),
		ServiceID: q.Get("service"),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"incidents": incidents,
		"total":     len(incidents),
	})
}

func (s *Server) createIncident(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ServiceID string `json:"service_id"`
		Title     string `json:"title"`
		Severity  string `json:"severity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if body.Title == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "title is required")
		return
	}
	if body.Severity != "" && body.Severity != "minor" && body.Severity != "major" {
		writeErr(w, http.StatusBadRequest, "bad_request", "severity must be minor|major")
		return
	}
	id, err := s.store.CreateIncident(r.Context(), body.ServiceID, body.Title, body.Severity, s.now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store write failed")
		return
	}
	incident, err := s.store.GetIncident(r.Context(), id)
	if err != nil || incident == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	w.Header().Set("Location", prefix+"/incidents/"+strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusCreated, incident)
}

func parseIncidentID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func (s *Server) getIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIncidentID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid incident id")
		return
	}
	incident, err := s.store.GetIncident(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	if incident == nil {
		writeErr(w, http.StatusNotFound, "not_found", "unknown incident")
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (s *Server) updateIncident(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIncidentID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid incident id")
		return
	}
	var body struct {
		Title    string `json:"title"`
		Severity string `json:"severity"`
		State    string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if body.Title == "" && body.Severity == "" && body.State == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "empty patch: set title, severity or state")
		return
	}
	if body.Severity != "" && body.Severity != "minor" && body.Severity != "major" {
		writeErr(w, http.StatusBadRequest, "bad_request", "severity must be minor|major")
		return
	}
	switch body.State {
	case "", "investigating", "monitoring", "resolved":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "state must be investigating|monitoring|resolved")
		return
	}
	if err := s.store.UpdateIncident(r.Context(), id, body.Title, body.Severity, body.State, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "unknown incident")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", "store write failed")
		return
	}
	incident, err := s.store.GetIncident(r.Context(), id)
	if err != nil || incident == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	writeJSON(w, http.StatusOK, incident)
}

func (s *Server) addUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIncidentID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid incident id")
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeDecodeErr(w, err)
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "text is required")
		return
	}
	if err := s.store.AddIncidentUpdate(r.Context(), id, body.Text, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "unknown incident")
			return
		}
		if errors.Is(err, store.ErrInvalid) {
			writeErr(w, http.StatusBadRequest, "bad_request", "text must be 1..2000 characters")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", "store write failed")
		return
	}
	incident, err := s.store.GetIncident(r.Context(), id)
	if err != nil || incident == nil {
		writeErr(w, http.StatusInternalServerError, "internal", "store read failed")
		return
	}
	w.Header().Set("Location", prefix+"/incidents/"+strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusCreated, incident)
}

// Log decorates h with one request line. Kept tiny on purpose.
func Log(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		log.Printf("epmon: %s %s", r.Method, r.URL.Path)
	})
}
