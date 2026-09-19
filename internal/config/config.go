// Package config loads epmon's declarative configuration from YAML or JSON.
//
// Format is detected from the file extension (.json vs YAML). Raw bytes
// first pass through ${VAR} substitution (§4.2), then strict decoding
// (unknown keys are errors), then defaults and validation (§4.3).
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	pkgstore "github.com/epmon-dev/epmon/pkg/store"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration with YAML/JSON string support ("30s", "5m").
type Duration time.Duration

// UnmarshalYAML decodes a duration string (strict: numbers rejected).
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("invalid duration (line %d): must be a string like \"30s\"", value.Line)
	}
	return d.parse(s, fmt.Sprintf("line %d", value.Line))
}

// UnmarshalJSON decodes a duration string (strict: numbers rejected).
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("invalid duration %s: must be a string like \"30s\"", strings.TrimSpace(string(data)))
	}
	return d.parse(s, "")
}

func (d *Duration) parse(s, where string) error {
	v, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		if where != "" {
			return fmt.Errorf("invalid duration %q (%s): %v", s, where, err)
		}
		return fmt.Errorf("invalid duration %q: %v", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalYAML renders the duration in Go syntax.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// MarshalJSON renders the duration in Go syntax.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

var (
	slugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	classRe = regexp.MustCompile(`^[1-5]xx$`)
	intRe   = regexp.MustCompile(`^[0-9]+$`)
)

// StatusSpec is a normalized set of expected HTTP statuses (§4.3.1).
// At load, class strings expand, the set dedupes and sorts, and Matches
// answers in O(1) via the precomputed table. Only the canonical []int is
// ever persisted or served.
type StatusSpec struct {
	Codes []int
	match [600]bool
}

// Matches reports whether code is expected. Codes outside [0,600) never match.
func (s StatusSpec) Matches(code int) bool {
	if code < 0 || code >= 600 {
		return false
	}
	return s.match[code]
}

// Normalized returns the canonical sorted code list.
func (s StatusSpec) Normalized() []int {
	out := append([]int(nil), s.Codes...)
	sort.Ints(out)
	return out
}

// MarshalYAML emits the normalized integer array.
func (s StatusSpec) MarshalYAML() (any, error) { return s.Normalized(), nil }

// MarshalJSON emits the normalized integer array.
func (s StatusSpec) MarshalJSON() ([]byte, error) { return json.Marshal(s.Normalized()) }

// UnmarshalYAML implements §4.3.1 strict parsing with file:line errors.
func (s *StatusSpec) UnmarshalYAML(node *yaml.Node) error {
	var raw any
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("expect_status: int, \"Nxx\", or array (line %d)", node.Line)
	}
	set := map[int]bool{}
	add := func(v int) error {
		if v < 100 || v > 599 {
			return fmt.Errorf("expect_status: %d outside [100,599] (line %d)", v, node.Line)
		}
		set[v] = true
		return nil
	}
	expand := func(cls string) error {
		if intRe.MatchString(cls) {
			return fmt.Errorf("expect_status: quoted integer %q must be unquoted (line %d)", cls, node.Line)
		}
		if !classRe.MatchString(cls) {
			return fmt.Errorf("expect_status: malformed class %q, want \"Nxx\" (line %d)", cls, node.Line)
		}
		base := int(cls[0]-'0') * 100
		for v := base; v < base+100; v++ {
			set[v] = true
		}
		return nil
	}
	elem := func(e any) error {
		switch t := e.(type) {
		case int:
			return add(t)
		case string:
			return expand(t)
		default:
			return fmt.Errorf("expect_status: int, \"Nxx\", or array (line %d)", node.Line)
		}
	}
	switch t := raw.(type) {
	case nil:
		return nil
	case int:
		if err := add(t); err != nil {
			return err
		}
	case string:
		if err := expand(t); err != nil {
			return err
		}
	case []any:
		if len(t) == 0 {
			return nil
		}
		for _, e := range t {
			if err := elem(e); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("expect_status: int, \"Nxx\", or array (line %d)", node.Line)
	}
	return s.freeze(set)
}

// UnmarshalJSON implements §4.3.1 strict parsing for JSON configs.
func (s *StatusSpec) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		return nil
	}
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("expect_status: int, \"Nxx\", or array: %v", err)
	}
	set := map[int]bool{}
	add := func(v int) error {
		if v < 100 || v > 599 {
			return fmt.Errorf("expect_status: %d outside [100,599]", v)
		}
		set[v] = true
		return nil
	}
	expand := func(cls string) error {
		if intRe.MatchString(cls) {
			return fmt.Errorf("expect_status: quoted integer %q must be unquoted", cls)
		}
		if !classRe.MatchString(cls) {
			return fmt.Errorf("expect_status: malformed class %q, want \"Nxx\"", cls)
		}
		base := int(cls[0]-'0') * 100
		for v := base; v < base+100; v++ {
			set[v] = true
		}
		return nil
	}
	num := func(n json.Number) (int, error) {
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("expect_status: %q is not an integer", n.String())
		}
		if json.Number(fmt.Sprint(i)).String() != n.String() {
			return 0, fmt.Errorf("expect_status: %q is not an integer", n.String())
		}
		return int(i), nil
	}
	switch t := raw.(type) {
	case json.Number:
		v, err := num(t)
		if err != nil {
			return err
		}
		if err := add(v); err != nil {
			return err
		}
	case string:
		if err := expand(t); err != nil {
			return err
		}
	case []any:
		for _, e := range t {
			switch et := e.(type) {
			case json.Number:
				v, err := num(et)
				if err != nil {
					return err
				}
				if err := add(v); err != nil {
					return err
				}
			case string:
				if err := expand(et); err != nil {
					return err
				}
			default:
				return fmt.Errorf("expect_status: int, \"Nxx\", or array")
			}
		}
	default:
		return fmt.Errorf("expect_status: int, \"Nxx\", or array")
	}
	return s.freeze(set)
}

func (s *StatusSpec) freeze(set map[int]bool) error {
	s.Codes = make([]int, 0, len(set))
	for v := range set {
		s.Codes = append(s.Codes, v)
	}
	sort.Ints(s.Codes)
	for _, v := range s.Codes {
		s.match[v] = true
	}
	return nil
}

// Service is a single monitored endpoint.
type Service struct {
	ID                 string            `yaml:"id" json:"id"`
	Name               string            `yaml:"name" json:"name"`
	URL                string            `yaml:"url" json:"url"`
	Method             string            `yaml:"method" json:"method"`
	Interval           Duration          `yaml:"interval" json:"interval"`
	Timeout            Duration          `yaml:"timeout" json:"timeout"`
	ExpectStatus       StatusSpec        `yaml:"expect_status" json:"expect_status"`
	Headers            map[string]string `yaml:"headers" json:"headers"`
	BodyContains       string            `yaml:"body_contains" json:"body_contains"`
	FollowRedirects    *bool             `yaml:"follow_redirects" json:"follow_redirects"`
	InsecureSkipVerify bool              `yaml:"insecure_skip_verify" json:"insecure_skip_verify"`
	FailureThreshold   int               `yaml:"failure_threshold" json:"failure_threshold"`
	Enabled            *bool             `yaml:"enabled" json:"enabled"`
	Aliases            []string          `yaml:"aliases" json:"aliases"`
	// MaxBodyBytes caps body_contains inspection for this service. It is
	// resolved from probes.max_body_bytes at load and hidden from file
	// formats (yaml/json "-") so the fleet knob stays the only source.
	MaxBodyBytes int64 `yaml:"-" json:"-"`
}

// TLSSkipVerify is the legacy alias for InsecureSkipVerify.
func (s Service) TLSSkipVerify() bool { return s.InsecureSkipVerify }

// FollowRedirectsOrDefault resolves the default-true flag.
func (s Service) FollowRedirectsOrDefault() bool {
	if s.FollowRedirects == nil {
		return true
	}
	return *s.FollowRedirects
}

// EnabledOrDefault resolves the default-true flag.
func (s Service) EnabledOrDefault() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// TLSConfig holds optional built-in TLS termination (min 1.2).
type TLSConfig struct {
	CertFile string `yaml:"cert_file" json:"cert_file"`
	KeyFile  string `yaml:"key_file" json:"key_file"`
}

// StatusPageConfig controls the embedded UI at /.
type StatusPageConfig struct {
	Enabled *bool `yaml:"enabled" json:"enabled"`
}

// MetricsConfig controls /metrics exposure.
type MetricsConfig struct {
	RequireAuth bool `yaml:"require_auth" json:"require_auth"`
}

// Server controls the HTTP API surface.
type Server struct {
	Listen       string           `yaml:"listen" json:"listen"`
	Addr         string           `yaml:"addr" json:"addr"`
	TLS          TLSConfig        `yaml:"tls" json:"tls"`
	ReadTimeout  Duration         `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout Duration         `yaml:"write_timeout" json:"write_timeout"`
	StatusPage   StatusPageConfig `yaml:"status_page" json:"status_page"`
	Metrics      MetricsConfig    `yaml:"metrics" json:"metrics"`
	CORS         CORSConfig       `yaml:"cors" json:"cors"`

	APIKeys            []string `yaml:"api_keys" json:"api_keys"`
	RateLimitRPM       int      `yaml:"rate_limit_rpm" json:"rate_limit_rpm"`
	RateLimitBurst     int      `yaml:"rate_limit_burst" json:"rate_limit_burst"`
	TrustProxy         bool     `yaml:"trust_proxy" json:"trust_proxy"`
	CORSAllowedOrigins []string `yaml:"cors_allowed_origins" json:"cors_allowed_origins"`
	MaxBodyBytes       int64    `yaml:"max_body_bytes" json:"max_body_bytes"`
	TLSCert            string   `yaml:"tls_cert" json:"tls_cert"`
	TLSKey             string   `yaml:"tls_key" json:"tls_key"`
}

// CORSConfig holds exact-match origins ([] = disabled).
type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowed_origins"`
}

// Storage tunes the bundled database adapter. database.dsn selects the
// file; only the adapter-relevant budgets live here.
type Storage struct {
	BusyTimeoutMs int `yaml:"busy_timeout_ms" json:"busy_timeout_ms"`
}

// History controls day-bucket timezone.
type History struct {
	Timezone string `yaml:"timezone" json:"timezone"`
}

// API controls auth and pagination. Rate limiting lives under server
// (server.rate_limit_rpm/burst); there is no api.rate_limit key.
type API struct {
	AuthTokens  []string `yaml:"auth_tokens" json:"auth_tokens"`
	MaxPageSize int      `yaml:"max_page_size" json:"max_page_size"`
}

// Probes holds fleet-wide probe defaults.
type Probes struct {
	DefaultInterval  Duration `yaml:"default_interval" json:"default_interval"`
	DefaultTimeout   Duration `yaml:"default_timeout" json:"default_timeout"`
	FailureThreshold int      `yaml:"failure_threshold" json:"failure_threshold"`
	Concurrency      int      `yaml:"concurrency" json:"concurrency"`
	MaxBodyBytes     int64    `yaml:"max_body_bytes" json:"max_body_bytes"`
	AutoIncidents    *bool    `yaml:"auto_incidents" json:"auto_incidents"`
}

// Incidents controls automatic resolution.
type Incidents struct {
	AutoResolve *bool `yaml:"auto_resolve" json:"auto_resolve"`
}

// Logging controls verbosity and encoding.
type Logging struct {
	Level  string `yaml:"level" json:"level"`
	Format string `yaml:"format" json:"format"`
}

// Database selects the storage adapter by name and hands it a DSN.
type Database struct {
	Driver        string `yaml:"driver" json:"driver"`
	DSN           string `yaml:"dsn" json:"dsn"`
	RetentionDays int    `yaml:"retention_days" json:"retention_days"`
}

// Config is the full epmon configuration.
type Config struct {
	Server    Server    `yaml:"server" json:"server"`
	Storage   Storage   `yaml:"storage" json:"storage"`
	History   History   `yaml:"history" json:"history"`
	API       API       `yaml:"api" json:"api"`
	Probes    Probes    `yaml:"probes" json:"probes"`
	Incidents Incidents `yaml:"incidents" json:"incidents"`
	Logging   Logging   `yaml:"logging" json:"logging"`
	Database  Database  `yaml:"database" json:"database"`
	Services  []Service `yaml:"services" json:"services"`
}

// AutoIncidentsOrDefault resolves the default-true flag.
func (c *Config) AutoIncidentsOrDefault() bool {
	if c.Probes.AutoIncidents == nil {
		return true
	}
	return *c.Probes.AutoIncidents
}

// AutoResolveOrDefault resolves the default-true flag.
func (c *Config) AutoResolveOrDefault() bool {
	if c.Incidents.AutoResolve == nil {
		return true
	}
	return *c.Incidents.AutoResolve
}

// StatusPageEnabled resolves the default-true flag.
func (c *Config) StatusPageEnabled() bool {
	if c.Server.StatusPage.Enabled == nil {
		return true
	}
	return *c.Server.StatusPage.Enabled
}

// Location resolves history.timezone (validated at load).
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.History.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// DiscoverPath implements §4.1: --config flag, EPMON_CONFIG, ./epmon.yaml,
// /etc/epmon/epmon.yaml. Empty means "defaults, zero services".
func DiscoverPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if v := os.Getenv("EPMON_CONFIG"); v != "" {
		return v
	}
	for _, p := range []string{"./epmon.yaml", "/etc/epmon/epmon.yaml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Default returns built-in defaults with zero services.
func Default() *Config {
	t := true
	cfg := &Config{}
	cfg.Server.Listen = ":8080"
	cfg.Server.ReadTimeout = Duration(10 * time.Second)
	cfg.Server.WriteTimeout = Duration(10 * time.Second)
	cfg.Server.StatusPage.Enabled = &t
	cfg.Storage.BusyTimeoutMs = 5000
	cfg.History.Timezone = "UTC"
	cfg.API.MaxPageSize = 100
	cfg.Probes.DefaultInterval = Duration(60 * time.Second)
	cfg.Probes.DefaultTimeout = Duration(10 * time.Second)
	cfg.Probes.FailureThreshold = 1
	cfg.Probes.Concurrency = 64
	cfg.Probes.MaxBodyBytes = 1 << 20
	cfg.Probes.AutoIncidents = &t
	cfg.Incidents.AutoResolve = &t
	cfg.Logging.Level = "info"
	cfg.Logging.Format = "json"
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = "epmon.db"
	cfg.Database.RetentionDays = 90
	cfg.Server.RateLimitRPM = 120
	cfg.Server.RateLimitBurst = 120
	cfg.Server.MaxBodyBytes = 1 << 20
	return cfg
}

// Load reads, substitutes, parses and validates path.
// .json selects JSON, anything else YAML.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(path, raw)
}

// Parse substitutes, decodes (strict) and validates in-memory bytes.
// extHint selects JSON when it ends in .json.
func Parse(filename string, raw []byte) (*Config, error) {
	sub, err := substituteEnv(raw, filename)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	cfg.Services = nil
	if strings.EqualFold(filepath.Ext(filename), ".json") {
		dec := json.NewDecoder(bytes.NewReader(sub))
		dec.DisallowUnknownFields()
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: parse JSON config: %w", filename, err)
		}
	} else {
		dec := yaml.NewDecoder(bytes.NewReader(sub))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: parse YAML config: %w", filename, err)
		}
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func boolPtr(b bool) *bool { v := b; return &v }

func (c *Config) applyDefaults() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.ReadTimeout.Std() == 0 {
		c.Server.ReadTimeout = Duration(10 * time.Second)
	}
	if c.Server.WriteTimeout.Std() == 0 {
		c.Server.WriteTimeout = Duration(10 * time.Second)
	}
	if c.Server.StatusPage.Enabled == nil {
		c.Server.StatusPage.Enabled = boolPtr(true)
	}
	if c.Storage.BusyTimeoutMs == 0 {
		c.Storage.BusyTimeoutMs = 5000
	}
	if c.History.Timezone == "" {
		c.History.Timezone = "UTC"
	}
	if c.API.MaxPageSize == 0 {
		c.API.MaxPageSize = 100
	}
	if c.Probes.DefaultInterval.Std() == 0 {
		c.Probes.DefaultInterval = Duration(60 * time.Second)
	}
	if c.Probes.DefaultTimeout.Std() == 0 {
		c.Probes.DefaultTimeout = Duration(10 * time.Second)
	}
	if c.Probes.FailureThreshold == 0 {
		c.Probes.FailureThreshold = 1
	}
	if c.Probes.Concurrency == 0 {
		c.Probes.Concurrency = 64
	}
	if c.Probes.MaxBodyBytes == 0 {
		c.Probes.MaxBodyBytes = 1 << 20
	}
	if c.Probes.AutoIncidents == nil {
		c.Probes.AutoIncidents = boolPtr(true)
	}
	if c.Incidents.AutoResolve == nil {
		c.Incidents.AutoResolve = boolPtr(true)
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Server.Addr == "" {
		c.Server.Addr = c.Server.Listen
	}
	if c.Server.Listen == "" || c.Server.Listen == ":8080" {
		if c.Server.Addr != "" {
			c.Server.Listen = c.Server.Addr
		}
	}
	if c.Server.RateLimitBurst == 0 {
		c.Server.RateLimitBurst = c.Server.RateLimitRPM
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = c.Probes.MaxBodyBytes
	}
	if c.Server.TLS.CertFile != "" {
		c.Server.TLSCert = c.Server.TLS.CertFile
		c.Server.TLSKey = c.Server.TLS.KeyFile
	}
	if c.Server.CORS.AllowedOrigins != nil && c.Server.CORSAllowedOrigins == nil {
		c.Server.CORSAllowedOrigins = c.Server.CORS.AllowedOrigins
	}
	if c.Server.TLSCert != "" || c.Server.TLSKey != "" {
		c.Server.TLS.CertFile = c.Server.TLSCert
		c.Server.TLS.KeyFile = c.Server.TLSKey
	}
	// Drop blank tokens (e.g. empty fallback expansions) — an empty key
	// would match an empty bearer and silently disable auth.
	kept := make([]string, 0, len(c.API.AuthTokens)+len(c.Server.APIKeys))
	for _, k := range c.API.AuthTokens {
		if strings.TrimSpace(k) != "" {
			kept = append(kept, k)
		}
	}
	for _, k := range c.Server.APIKeys {
		if strings.TrimSpace(k) != "" {
			kept = append(kept, k)
		}
	}
	c.API.AuthTokens = kept
	c.Server.APIKeys = append([]string(nil), c.API.AuthTokens...)
	for i := range c.Services {
		s := &c.Services[i]
		if s.Method == "" {
			s.Method = "GET"
		}
		if s.Interval.Std() == 0 {
			s.Interval = c.Probes.DefaultInterval
		}
		if s.Timeout.Std() == 0 {
			s.Timeout = c.Probes.DefaultTimeout
		}
		if len(s.ExpectStatus.Codes) == 0 {
			d := StatusSpec{}
			_ = d.freeze(map[int]bool{200: true})
			s.ExpectStatus = d
		}
		if s.FollowRedirects == nil {
			s.FollowRedirects = boolPtr(true)
		}
		if s.Enabled == nil {
			s.Enabled = boolPtr(true)
		}
		if s.FailureThreshold == 0 {
			s.FailureThreshold = c.Probes.FailureThreshold
		}
		if s.MaxBodyBytes == 0 {
			s.MaxBodyBytes = c.Probes.MaxBodyBytes
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		if s.Headers == nil {
			s.Headers = map[string]string{}
		}
		// Canonicalize header names on load.
		canon := make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			canon[textproto.CanonicalMIMEHeaderKey(k)] = v
		}
		s.Headers = canon
		if s.InsecureSkipVerify {
			warnOnce("insecure-skip-verify:" + s.ID)
		}
	}
	// Reserved knobs: validated but not yet honored by the runtime. Warn
	// when explicitly set so operators don't silently tune dead settings.
	// (applyDefaults already filled defaults above, so any non-default
	// value here came from the file.)
	if c.Logging.Level != "info" || c.Logging.Format != "json" {
		warnOncef("unimplemented:logging", "logging.level/format have no effect yet (log output is fixed)")
	}
	if c.Probes.AutoIncidents != nil && !*c.Probes.AutoIncidents {
		warnOncef("unimplemented:auto-incidents", "probes.auto_incidents=false has no effect yet (no automatic incidents exist)")
	}
	if c.Incidents.AutoResolve != nil && !*c.Incidents.AutoResolve {
		warnOncef("unimplemented:auto-resolve", "incidents.auto_resolve=false has no effect yet")
	}
	if c.Server.StatusPage.Enabled != nil && !*c.Server.StatusPage.Enabled {
		warnOncef("unimplemented:status-page", "server.status_page.enabled=false has no effect yet")
	}
	if c.Server.Metrics.RequireAuth {
		warnOncef("unimplemented:metrics-auth", "server.metrics.require_auth=true has no effect yet (/metrics stays public)")
	}
	return nil
}

var warnOnceSeen sync.Map

func warnOnce(key string) {
	if _, loaded := warnOnceSeen.LoadOrStore(key, true); loaded {
		return
	}
	fmt.Fprintf(os.Stderr, "epmon: WARNING: %s: insecure_skip_verify skips TLS verification\n", key)
}

// warnOncef emits a process-once stderr warning for key.
func warnOncef(key, format string, args ...any) {
	if _, loaded := warnOnceSeen.LoadOrStore(key, true); loaded {
		return
	}
	fmt.Fprintf(os.Stderr, "epmon: WARNING: "+format+"\n", args...)
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// Validate enforces every constraint in §4.3. Zero services is valid
// (idle defaults); bounds and cross-field rules are hard errors.
func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("server.listen must be host:port, got %q", c.Server.Listen)
	}
	if (c.Server.TLS.CertFile == "") != (c.Server.TLS.KeyFile == "") {
		return fmt.Errorf("server.tls.cert_file and server.tls.key_file must be set together")
	}
	if c.Server.ReadTimeout.Std() < time.Second {
		return fmt.Errorf("server.read_timeout must be >= 1s")
	}
	if c.Server.WriteTimeout.Std() < time.Second {
		return fmt.Errorf("server.write_timeout must be >= 1s")
	}
	if c.Storage.BusyTimeoutMs < 100 || c.Storage.BusyTimeoutMs > 60000 {
		return fmt.Errorf("storage.busy_timeout_ms must be 100..60000")
	}
	if _, err := time.LoadLocation(c.History.Timezone); err != nil {
		return fmt.Errorf("history.timezone: unknown IANA name %q", c.History.Timezone)
	}
	if c.API.MaxPageSize < 10 || c.API.MaxPageSize > 1000 {
		return fmt.Errorf("api.max_page_size must be 10..1000")
	}
	if c.Database.RetentionDays < 1 {
		return fmt.Errorf("database.retention_days must be >= 1")
	}
	if strings.TrimSpace(c.Database.Driver) == "" {
		return fmt.Errorf("database.driver is required")
	}
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.Server.RateLimitRPM < 0 || c.Server.RateLimitBurst < 1 {
		return fmt.Errorf("server.rate_limit_rpm must be >= 0 and rate_limit_burst >= 1")
	}
	if c.Server.MaxBodyBytes < 1 {
		return fmt.Errorf("server.max_body_bytes must be >= 1")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("config defines no services")
	}
	if c.Server.MaxBodyBytes < 1 {
		return fmt.Errorf("server.max_body_bytes must be >= 1")
	}
	if iv := c.Probes.DefaultInterval.Std(); iv < 5*time.Second || iv > 24*time.Hour {
		return fmt.Errorf("probes.default_interval must be 5s..24h")
	}
	if to := c.Probes.DefaultTimeout.Std(); to < time.Second || to >= c.Probes.DefaultInterval.Std() {
		return fmt.Errorf("probes.default_timeout must be >= 1s and < default_interval")
	}
	if c.Probes.FailureThreshold < 1 {
		return fmt.Errorf("probes.failure_threshold must be >= 1")
	}
	if c.Probes.Concurrency < 1 || c.Probes.Concurrency > 1024 {
		return fmt.Errorf("probes.concurrency must be 1..1024")
	}
	if c.Probes.MaxBodyBytes < 1 || c.Probes.MaxBodyBytes > 16<<20 {
		return fmt.Errorf("probes.max_body_bytes must be 1..16777216")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level must be debug|info|warn|error")
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		return fmt.Errorf("logging.format must be json|text")
	}
	if len(c.Services) > 1000 {
		return fmt.Errorf("services: at most 1000 entries, got %d", len(c.Services))
	}
	seen := map[string]int{}
	aliasOwners := map[string]string{}
	live := map[string]bool{}
	for i := range c.Services {
		s := &c.Services[i]
		if !slugRe.MatchString(s.ID) {
			return fmt.Errorf("services[%d]: id %q must match ^[a-z0-9][a-z0-9_-]{0,63}$", i, s.ID)
		}
		if prev, dup := seen[s.ID]; dup {
			return fmt.Errorf("duplicate service id %q (services[%d] and services[%d])", s.ID, prev, i)
		}
		seen[s.ID] = i
		live[s.ID] = true
		if n := len([]rune(s.Name)); n < 1 || n > 128 {
			return fmt.Errorf("service %q: name must be 1..128 chars", s.ID)
		}
		u, err := url.Parse(s.URL)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("service %q: url must be absolute http(s), got %q", s.ID, s.URL)
		}
		switch s.Method {
		case "GET", "HEAD", "POST":
		default:
			return fmt.Errorf("service %q: method must be GET|HEAD|POST", s.ID)
		}
		if len(s.ExpectStatus.Codes) == 0 {
			return fmt.Errorf("service %q: expect_status must list at least one status", s.ID)
		}
		if iv := s.Interval.Std(); iv < 5*time.Second || iv > 24*time.Hour {
			return fmt.Errorf("service %q: interval must be 5s..24h", s.ID)
		}
		if to := s.Timeout.Std(); to < time.Second || to >= s.Interval.Std() {
			return fmt.Errorf("service %q: timeout must be >= 1s and < interval", s.ID)
		}
		if s.FailureThreshold < 1 {
			return fmt.Errorf("service %q: failure_threshold must be >= 1", s.ID)
		}
		for _, a := range s.Aliases {
			if !slugRe.MatchString(a) {
				return fmt.Errorf("service %q: alias %q must match ^[a-z0-9][a-z0-9_-]{0,63}$", s.ID, a)
			}
			if owner, dup := aliasOwners[a]; dup {
				return fmt.Errorf("alias %q declared by both %q and %q", a, owner, s.ID)
			}
			aliasOwners[a] = s.ID
		}
	}
	for alias, owner := range aliasOwners {
		if live[alias] {
			return fmt.Errorf("service %q: alias %q equals a live service id", owner, alias)
		}
	}
	return nil
}

// substituteEnv implements §4.2 over raw bytes before parsing.
// $$ is a literal $. Missing vars are fatal, all reported with file:line.
func substituteEnv(raw []byte, filename string) ([]byte, error) {
	const nulPlaceholder = "\x00"
	s := string(raw)
	var out strings.Builder
	out.Grow(len(s))
	type missing struct {
		name string
		line int
	}
	var miss []missing
	var bare []missing
	line := 1
	i := 0
	for i < len(s) {
		ch := s[i]
		if ch == '\n' {
			out.WriteByte(ch)
			line++
			i++
			continue
		}
		if ch == '$' && i+1 < len(s) && s[i+1] == '$' {
			out.WriteString(nulPlaceholder)
			i += 2
			continue
		}
		if ch == '$' && i+1 < len(s) && s[i+1] == '{' {
			rest := s[i+2:]
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				return nil, fmt.Errorf("%s: line %d: unterminated ${ substitution", filename, line)
			}
			inner := rest[:end]
			name, fallback, hasFallback := strings.Cut(inner, ":-")
			if !validEnvName(name) {
				return nil, fmt.Errorf("%s: line %d: malformed substitution ${%s}", filename, line, inner)
			}
			val, ok := os.LookupEnv(name)
			if hasFallback && (!ok || val == "") {
				val = fallback
				ok = true
			}
			if !ok {
				miss = append(miss, missing{name, line})
			} else {
				out.WriteString(val)
				line += strings.Count(val, "\n")
			}
			i += 2 + end + 1
			continue
		}
		if ch == '$' && i+1 < len(s) && isNameStart(s[i+1]) {
			// A plausible bare $NAME reference: left untouched (only
			// ${...} expands), but worth a warning — a literal secret
			// name here is usually a missing pair of braces.
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			name := s[i+1 : j]
			seen := false
			for _, b := range bare {
				if b.name == name {
					seen = true
					break
				}
			}
			if !seen {
				bare = append(bare, missing{name, line})
			}
		}
		out.WriteByte(ch)
		i++
	}
	if len(miss) > 0 {
		parts := make([]string, 0, len(miss))
		for _, m := range miss {
			parts = append(parts, fmt.Sprintf("%s (line %d)", m.name, m.line))
		}
		return nil, fmt.Errorf("%s: missing required environment variables: %s", filename, strings.Join(parts, ", "))
	}
	for _, b := range bare {
		warnOncef("bare-substitution:"+b.name, "%s: line %d: bare $%s left untouched (only ${...} expands)", filename, b.line, b.name)
	}
	return []byte(strings.ReplaceAll(out.String(), nulPlaceholder, "$")), nil
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// ConfigDiff is the hot-reload delta matched by services[].id (§4.4).
type ConfigDiff struct {
	Added     []Service
	Removed   []Service
	Changed   []Service
	Unchanged []Service
}

// DiffConfigs diffs old (running) against new (reloaded).
func DiffConfigs(old, new *Config) ConfigDiff {
	var d ConfigDiff
	oldByID := map[string]Service{}
	for _, s := range old.Services {
		oldByID[s.ID] = s
	}
	newByID := map[string]Service{}
	for _, s := range new.Services {
		newByID[s.ID] = s
		if o, ok := oldByID[s.ID]; !ok {
			d.Added = append(d.Added, s)
		} else if servicesEqual(o, s) {
			d.Unchanged = append(d.Unchanged, s)
		} else {
			d.Changed = append(d.Changed, s)
		}
	}
	for _, s := range old.Services {
		if _, ok := newByID[s.ID]; !ok {
			d.Removed = append(d.Removed, s)
		}
	}
	return d
}

func servicesEqual(a, b Service) bool {
	if a.ID != b.ID || a.Name != b.Name || a.URL != b.URL || a.Method != b.Method ||
		a.Interval.Std() != b.Interval.Std() || a.Timeout.Std() != b.Timeout.Std() ||
		a.BodyContains != b.BodyContains ||
		a.FollowRedirectsOrDefault() != b.FollowRedirectsOrDefault() ||
		a.InsecureSkipVerify != b.InsecureSkipVerify ||
		a.FailureThreshold != b.FailureThreshold ||
		a.EnabledOrDefault() != b.EnabledOrDefault() ||
		len(a.ExpectStatus.Codes) != len(b.ExpectStatus.Codes) ||
		len(a.Headers) != len(b.Headers) || len(a.Aliases) != len(b.Aliases) {
		return false
	}
	for i := range a.ExpectStatus.Codes {
		if a.ExpectStatus.Codes[i] != b.ExpectStatus.Codes[i] {
			return false
		}
	}
	for k, v := range a.Headers {
		if bv, ok := b.Headers[k]; !ok || bv != v {
			return false
		}
	}
	aa := append([]string(nil), a.Aliases...)
	bb := append([]string(nil), b.Aliases...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// Snapshot converts a service to its persisted row. CreatedAt is stamped
// by the store on insert (0 = new).
func (s Service) Snapshot() pkgstore.ServiceSnapshot {
	expect, _ := json.Marshal(s.ExpectStatus.Normalized())
	headers, _ := json.Marshal(s.Headers)
	return pkgstore.ServiceSnapshot{
		ID:               s.ID,
		Name:             s.Name,
		URL:              s.URL,
		Method:           s.Method,
		IntervalSeconds:  int64(s.Interval.Std() / time.Second),
		TimeoutSeconds:   int64(s.Timeout.Std() / time.Second),
		ExpectStatus:     string(expect),
		Headers:          string(headers),
		BodyContains:     s.BodyContains,
		FailureThreshold: s.FailureThreshold,
	}
}

// ToStoreDiff converts a ConfigDiff into the atomic actor command,
// attaching alias-migration operations (§4.5).
func (d ConfigDiff) ToStoreDiff() pkgstore.ConfigDiff {
	out := pkgstore.ConfigDiff{}
	for _, s := range d.Added {
		out.Upserts = append(out.Upserts, s.Snapshot())
	}
	for _, s := range d.Changed {
		out.Upserts = append(out.Upserts, s.Snapshot())
	}
	for _, s := range d.Removed {
		out.Archives = append(out.Archives, s.ID)
	}
	for _, s := range append(append([]Service{}, d.Added...), d.Changed...) {
		for _, a := range s.Aliases {
			out.Migrations = append(out.Migrations, pkgstore.MigrateServiceOp{From: a, To: s.ID})
		}
	}
	return out
}
