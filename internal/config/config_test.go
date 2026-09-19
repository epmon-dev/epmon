package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func mustSpec(t *testing.T, text string) StatusSpec {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("v: "+text+"\n"), &doc); err != nil {
		t.Fatalf("mustSpec(%q): %v", text, err)
	}
	var s StatusSpec
	if err := s.UnmarshalYAML(doc.Content[0].Content[1]); err != nil {
		t.Fatalf("mustSpec(%q): %v", text, err)
	}
	return s
}

func TestToStoreDiff(t *testing.T) {
	oldCfg := &Config{Services: []Service{
		{ID: "keep", Name: "keep", URL: "https://keep.example.com", Method: "GET", Interval: Duration(60 * time.Second), Timeout: Duration(10 * time.Second), ExpectStatus: mustSpec(t, "[200]"), FailureThreshold: 1},
		{ID: "gone", Name: "gone", URL: "https://gone.example.com", Method: "GET", Interval: Duration(60 * time.Second), Timeout: Duration(10 * time.Second), ExpectStatus: mustSpec(t, "[200]"), FailureThreshold: 1},
	}}
	newCfg := &Config{Services: []Service{
		{ID: "keep", Name: "keep", URL: "https://keep.example.com", Method: "GET", Interval: Duration(60 * time.Second), Timeout: Duration(10 * time.Second), ExpectStatus: mustSpec(t, "[200]"), FailureThreshold: 1},
		{ID: "new", Name: "new", URL: "https://new.example.com", Method: "GET", Interval: Duration(30 * time.Second), Timeout: Duration(5 * time.Second), ExpectStatus: mustSpec(t, "[200]"), FailureThreshold: 1, Aliases: []string{"gone"}},
	}}
	diff := DiffConfigs(oldCfg, newCfg).ToStoreDiff()
	if len(diff.Upserts) != 1 || diff.Upserts[0].ID != "new" {
		t.Fatalf("upserts = %+v", diff.Upserts)
	}
	if len(diff.Archives) != 1 || diff.Archives[0] != "gone" {
		t.Fatalf("archives = %+v", diff.Archives)
	}
	if len(diff.Migrations) != 1 || diff.Migrations[0].From != "gone" || diff.Migrations[0].To != "new" {
		t.Fatalf("migrations = %+v", diff.Migrations)
	}
	if diff.Upserts[0].IntervalSeconds != 30 || diff.Upserts[0].ExpectStatus != "[200]" {
		t.Fatalf("snapshot = %+v", diff.Upserts[0])
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadYAMLDefaults(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
services:
  - id: web
    url: https://example.com
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	svc := cfg.Services[0]
	if svc.Name != "web" {
		t.Errorf("Name default = %q, want id", svc.Name)
	}
	if svc.Method != "GET" || svc.Interval.Std() != time.Minute || svc.Timeout.Std() != 10*time.Second {
		t.Errorf("bad method/interval/timeout defaults: %+v", svc)
	}
	if len(svc.ExpectStatus.Codes) != 1 || svc.ExpectStatus.Codes[0] != 200 {
		t.Errorf("bad expect_status default: %v", svc.ExpectStatus.Codes)
	}
	if cfg.Server.Addr != ":8080" || cfg.Database.RetentionDays != 90 {
		t.Errorf("bad server/db defaults: %+v %+v", cfg.Server, cfg.Database)
	}
	if cfg.Database.DSN != "epmon.db" {
		t.Errorf("db dsn default = %q", cfg.Database.DSN)
	}
}

func TestLoadJSON(t *testing.T) {
	path := writeTemp(t, "config.json", `{
	  "services": [{"id": "a", "name": "A", "url": "http://localhost:1/", "interval": "30s"}]
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Services[0].Interval.Std() != 30*time.Second {
		t.Errorf("interval = %v", cfg.Services[0].Interval.Std())
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("EPMON_TEST_TOKEN", "s3cret")
	path := writeTemp(t, "config.yaml", `
services:
  - id: web
    url: https://example.com
    headers:
      Authorization: "Bearer ${EPMON_TEST_TOKEN}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Services[0].Headers["Authorization"]; got != "Bearer s3cret" {
		t.Errorf("Authorization = %q", got)
	}
}

// TestEnvSubstitutionForms locks the documented contract: ${VAR} and
// ${VAR:-fallback} expand, $$ collapses to $, and a bare $VAR is left
// untouched (with a stderr hint, asserted only by behavior here).
func TestEnvSubstitutionForms(t *testing.T) {
	t.Setenv("EPMON_FORM_TOKEN", "s3cret")
	t.Setenv("EPMON_FORM_EMPTY", "")
	path := writeTemp(t, "config.yaml", `
services:
  - id: web
    url: https://example.com
    headers:
      Braced: "Bearer ${EPMON_FORM_TOKEN}"
      Fallback: "Bearer ${EPMON_FORM_UNSET:-d3fault}"
      EmptyFallback: "Bearer ${EPMON_FORM_EMPTY:-d3fault}"
      Escaped: "price $$5"
      Bare: "Bearer $EPMON_FORM_TOKEN"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := cfg.Services[0].Headers
	for k, want := range map[string]string{
		"Braced":        "Bearer s3cret",
		"Fallback":      "Bearer d3fault",
		"Emptyfallback": "Bearer d3fault",
		"Escaped":       "price $5",
		"Bare":          "Bearer $EPMON_FORM_TOKEN",
	} {
		if h[k] != want {
			t.Errorf("%s = %q, want %q", k, h[k], want)
		}
	}
}

func TestServerValidation(t *testing.T) {
	cases := map[string]string{
		"tls half-set":    "server: {tls_cert: /c.pem}\nservices:\n  - {id: a, url: https://example.com}",
		"negative rpm":    "server: {rate_limit_rpm: -1}\nservices:\n  - {id: a, url: https://example.com}",
		"negative body":   "server: {max_body_bytes: -1}\nservices:\n  - {id: a, url: https://example.com}",
		"empty driver":    "database: {driver: '  '}\nservices:\n  - {id: a, url: https://example.com}",
		"server defaults": "services:\n  - {id: a, url: https://example.com}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, "config.yaml", body)
			cfg, err := Load(path)
			if name == "server defaults" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if cfg.Server.RateLimitRPM != 120 || cfg.Server.RateLimitBurst != 120 {
					t.Errorf("rate defaults = %d/%d", cfg.Server.RateLimitRPM, cfg.Server.RateLimitBurst)
				}
				if cfg.Database.Driver != "sqlite" || cfg.Database.DSN != "epmon.db" {
					t.Errorf("db defaults = %+v", cfg.Database)
				}
				return
			}
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestBlankAPIKeysDropped(t *testing.T) {
	// Unset ${VARS} must not become an empty key that matches empty tokens.
	t.Setenv("EPMON_TEST_UNSET", "")
	path := writeTemp(t, "config.yaml", `
server:
  api_keys: ["${EPMON_TEST_UNSET}", "  ", "real"]
services:
  - {id: a, url: https://example.com}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Server.APIKeys) != 1 || cfg.Server.APIKeys[0] != "real" {
		t.Errorf("APIKeys = %q", cfg.Server.APIKeys)
	}
}

func TestValidateEmptyExpectStatus(t *testing.T) {
	// Unreachable via Load (empty lists take the default); Validate still guards it.
	cfg := &Config{Services: []Service{{ID: "a", URL: "https://example.com"}}}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for empty expect_status")
	}
}

func TestLegacyServerFieldsBridge(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
server:
  addr: ":9443"
  api_keys: ["k1"]
  cors_allowed_origins: ["https://app.example.com"]
  tls_cert: "/c.pem"
  tls_key: "/k.pem"
database:
  dsn: "legacy.db"
services:
  - {id: a, url: https://example.com}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9443" || cfg.Server.Listen != ":9443" {
		t.Errorf("addr/listen = %q/%q", cfg.Server.Addr, cfg.Server.Listen)
	}
	if len(cfg.Server.APIKeys) != 1 || cfg.Server.APIKeys[0] != "k1" {
		t.Errorf("APIKeys = %q", cfg.Server.APIKeys)
	}
	if len(cfg.Server.CORSAllowedOrigins) != 1 || cfg.Server.CORSAllowedOrigins[0] != "https://app.example.com" {
		t.Errorf("CORSAllowedOrigins = %q", cfg.Server.CORSAllowedOrigins)
	}
	if cfg.Server.TLSCert != "/c.pem" || cfg.Server.TLSKey != "/k.pem" {
		t.Errorf("flat tls = %q/%q", cfg.Server.TLSCert, cfg.Server.TLSKey)
	}
	if cfg.Server.TLS.CertFile != "/c.pem" || cfg.Server.TLS.KeyFile != "/k.pem" {
		t.Errorf("nested tls = %q/%q", cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
	}
	if cfg.Database.DSN != "legacy.db" {
		t.Errorf("dsn = %q", cfg.Database.DSN)
	}
}

func TestStatusSpecClassExpansion(t *testing.T) {
	path := writeTemp(t, "config.yaml", `
services:
  - {id: a, url: https://example.com, expect_status: [200, "2xx", 404]}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec := cfg.Services[0].ExpectStatus
	if !spec.Matches(200) || !spec.Matches(299) || !spec.Matches(404) {
		t.Errorf("class expansion failed: %v", spec.Normalized())
	}
	if spec.Matches(500) || spec.Matches(99) || spec.Matches(600) {
		t.Errorf("unexpected match: %v", spec.Normalized())
	}
	if len(spec.Normalized()) != 101 {
		t.Errorf("normalized = %d codes, want 101 (200..299 + 404)", len(spec.Normalized()))
	}
}

func TestServiceTLSSkipVerifyAlias(t *testing.T) {
	svc := Service{InsecureSkipVerify: true}
	if !svc.TLSSkipVerify() {
		t.Error("TLSSkipVerify should alias InsecureSkipVerify")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"no services":       `server: {addr: ":1"}`,
		"duplicate id":      "services:\n  - {id: a, url: https://example.com}\n  - {id: a, url: https://example.com}",
		"missing id":        "services:\n  - {url: https://example.com}",
		"bad scheme":        "services:\n  - {id: a, url: ftp://example.com}",
		"relative url":      "services:\n  - {id: a, url: /healthz}",
		"negative interval": "services:\n  - {id: a, url: https://example.com, interval: -5s}",
		"zero retention":    "database: {retention_days: -5}\nservices:\n  - {id: a, url: https://example.com}",
		"bad duration":      "services:\n  - {id: a, url: https://example.com, interval: never}",
		"bad yaml":          "services: [unclosed",
		"missing file":      "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "missing file" {
				if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
					t.Error("expected error")
				}
				return
			}
			path := writeTemp(t, "config.yaml", body)
			if _, err := Load(path); err == nil {
				t.Error("expected error")
			}
		})
	}
}
