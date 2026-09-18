package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/store"
	_ "github.com/epmon-dev/epmon/internal/store/sqlite"
	"gopkg.in/yaml.v3"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), "sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func testServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	// Same construction path as production (registry by driver name).
	st := openTestStore(t)
	cfg := &config.Config{
		Server: config.Server{MaxBodyBytes: 1 << 20},
		Services: []config.Service{
			{ID: "web", Name: "Web", URL: "https://example.com"},
			{ID: "api", Name: "API", URL: "https://api.example.com/"},
		},
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	return New(cfg, st, func() time.Time { return now }), st
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any, http.Header) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s %s: invalid JSON %q: %v", method, path, rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s: Content-Type = %q", method, path, ct)
	}
	return rec.Code, decoded, rec.Header()
}

func TestStatusUnknownThenOutage(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Handler()
	ctx := t.Context()

	if code, body, _ := do(t, h, "GET", "/healthz", ""); code != 200 || body["ok"] != true {
		t.Errorf("healthz = %d %v", code, body)
	}
	if code, body, _ := do(t, h, "GET", "/api/v1/status", ""); code != 200 || body["overall"] != "unknown" {
		t.Errorf("fresh status = %d %v", code, body)
	}

	ts := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC).Unix()
	if err := st.RecordCheck(ctx, store.Check{ServiceID: "web", TS: ts, Up: true, LatencyMs: 42, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheck(ctx, store.Check{ServiceID: "api", TS: ts, StatusCode: 500, Error: "status: got 500"}); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, h, "GET", "/api/v1/status", "")
	if code != 200 || body["overall"] != "partial_outage" {
		t.Errorf("status = %d %v", code, body)
	}
	services := body["services"].([]any)
	if len(services) != 2 {
		t.Fatalf("services = %v", services)
	}
	web := services[0].(map[string]any)
	if web["up"] != true || web["latency_ms"] != float64(42) {
		t.Errorf("web state = %v", web)
	}

	code, body, _ = do(t, h, "GET", "/api/v1/services", "")
	if code != 200 || body["total"] != float64(2) {
		t.Errorf("services list = %d %v", code, body)
	}
	// Trailing slash tolerance.
	if code, _, _ := do(t, h, "GET", "/api/v1/services/", ""); code != 200 {
		t.Errorf("trailing slash = %d, want 200", code)
	}
}

func TestRoutingErrorsAreJSON(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Handler()

	for _, path := range []string{"/nope", "/api/v1/nope", "/api/v2/status", "/api/status"} {
		code, body, _ := do(t, h, "GET", path, "")
		if code != 404 {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok || errObj["code"] != "not_found" {
			t.Errorf("GET %s error envelope = %v", path, body)
		}
	}
}

func TestHistory(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Handler()
	ctx := t.Context()

	ts := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC).Unix()
	for i := 0; i < 4; i++ {
		if err := st.RecordCheck(ctx, store.Check{ServiceID: "web", TS: int64(i) + ts, Up: true, LatencyMs: 5, StatusCode: 200}); err != nil {
			t.Fatal(err)
		}
	}
	code, body, _ := do(t, h, "GET", "/api/v1/services/web/history?days=2", "")
	if code != 200 {
		t.Fatalf("history = %d", code)
	}
	if body["uptime_pct"] != float64(100) {
		t.Errorf("uptime = %v", body["uptime_pct"])
	}
	if len(body["history"].([]any)) != 2 || len(body["recent"].([]any)) != 4 || body["total"] != float64(4) {
		t.Errorf("history shape = %v", body)
	}

	code, _, _ = do(t, h, "GET", "/api/v1/services/web/history?limit=2", "")
	if code != 200 {
		t.Fatalf("history limit = %d", code)
	}
	code, body, _ = do(t, h, "GET", "/api/v1/services/nope/history", "")
	if code != 404 || body["error"].(map[string]any)["code"] != "not_found" {
		t.Errorf("unknown service = %d %v, want 404/not_found", code, body)
	}
	for _, bad := range []string{"?days=999", "?days=0", "?days=x", "?limit=0"} {
		if code, _, _ := do(t, h, "GET", "/api/v1/services/web/history"+bad, ""); code != 400 {
			t.Errorf("history%s = %d, want 400", bad, code)
		}
	}
}

func TestIncidentLifecycle(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Handler()

	code, created, header := do(t, h, "POST", "/api/v1/incidents", `{"service_id":"web","title":"Slow"}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, created)
	}
	id := int(created["id"].(float64))
	wantLocation := fmt.Sprintf("/api/v1/incidents/%d", id)
	if header.Get("Location") != wantLocation {
		t.Errorf("Location = %q, want %q", header.Get("Location"), wantLocation)
	}
	if created["state"] != "investigating" || created["severity"] != "minor" {
		t.Errorf("defaults = %v", created)
	}
	path := fmt.Sprintf("/api/v1/incidents/%d", id)

	if code, body, _ := do(t, h, "GET", path, ""); code != 200 || body["title"] != "Slow" {
		t.Errorf("get = %d %v", code, body)
	}
	if code, _, _ := do(t, h, "GET", "/api/v1/incidents/999999", ""); code != 404 {
		t.Errorf("get missing = %d, want 404", code)
	}
	if code, _, _ := do(t, h, "POST", "/api/v1/incidents", `{"severity":"major"}`); code != 400 {
		t.Errorf("create without title = %d, want 400", code)
	}
	if code, _, _ := do(t, h, "POST", "/api/v1/incidents", `{"title":"x","severity":"critical"}`); code != 400 {
		t.Errorf("create with bad severity = %d, want 400", code)
	}

	if code, _, _ := do(t, h, "POST", path+"/updates", `{"text":"Looking into it"}`); code != 201 {
		t.Errorf("add update = %d", code)
	}
	if code, _, _ := do(t, h, "POST", "/api/v1/incidents/999999/updates", `{"text":"x"}`); code != 404 {
		t.Errorf("update on missing = %d, want 404", code)
	}
	if code, _, _ := do(t, h, "PATCH", path, `{"state":"resolved"}`); code != 200 {
		t.Errorf("resolve = %d", code)
	}
	if code, _, _ := do(t, h, "PATCH", path, `{}`); code != 400 {
		t.Errorf("empty patch = %d, want 400", code)
	}
	if code, _, _ := do(t, h, "PATCH", path, `{"state":"bogus"}`); code != 400 {
		t.Errorf("bad state = %d, want 400", code)
	}
	if code, _, _ := do(t, h, "PATCH", path, `{"severity":"bogus"}`); code != 400 {
		t.Errorf("bad severity = %d, want 400", code)
	}
	if code, _, _ := do(t, h, "PATCH", "/api/v1/incidents/999999", `{"state":"resolved"}`); code != 404 {
		t.Errorf("patch missing = %d, want 404", code)
	}

	code, body, _ := do(t, h, "GET", "/api/v1/incidents?state=resolved", "")
	if code != 200 || body["total"] != float64(1) {
		t.Errorf("list resolved = %d %v", code, body)
	}
	incident := body["incidents"].([]any)[0].(map[string]any)
	if len(incident["updates"].([]any)) != 1 {
		t.Errorf("updates = %v", incident["updates"])
	}
	code, body, _ = do(t, h, "GET", "/api/v1/incidents?state=investigating", "")
	if code != 200 || body["total"] != float64(0) {
		t.Errorf("list investigating = %d %v", code, body)
	}
	code, body, _ = do(t, h, "GET", "/api/v1/incidents?service=web", "")
	if code != 200 || body["total"] != float64(1) {
		t.Errorf("filter service=web = %d %v", code, body)
	}
	code, body, _ = do(t, h, "GET", "/api/v1/incidents?service=other", "")
	if code != 200 || body["total"] != float64(0) {
		t.Errorf("filter service=other = %d %v", code, body)
	}

	// Update text is bounded to 1..2000 characters (runes), enforced in
	// the store and surfaced as 400 here.
	if code, body, _ := do(t, h, "POST", path+"/updates", fmt.Sprintf(`{"text":%q}`, strings.Repeat("a", 2001))); code != 400 || body["error"].(map[string]any)["code"] != "bad_request" {
		t.Errorf("oversize update = %d %v, want 400/bad_request", code, body)
	}
	if code, _, _ := do(t, h, "POST", path+"/updates", fmt.Sprintf(`{"text":%q}`, strings.Repeat("a", 2000))); code != 201 {
		t.Errorf("2000-char update = %d, want 201", code)
	}
}

func TestOpenAPISpecParity(t *testing.T) {
	srv, _ := testServer(t)
	h := srv.Handler()

	// YAML spec serves and parses.
	req := httptest.NewRequest("GET", "/api/v1/openapi.yaml", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "yaml") {
		t.Fatalf("openapi.yaml = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var doc map[string]any
	if err := yaml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("spec is not valid YAML: %v", err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("spec has no paths")
	}
	for _, p := range []string{
		"/healthz",
		"/api/v1/status",
		"/api/v1/services",
		"/api/v1/services/{id}/history",
		"/api/v1/incidents",
		"/api/v1/incidents/{id}",
		"/api/v1/incidents/{id}/updates",
		"/api/v1/openapi.yaml",
		"/api/v1/openapi.json",
		"/docs",
	} {
		if _, ok := paths[p]; !ok {
			t.Errorf("spec missing path %s", p)
		}
	}

	// JSON variant is the same document.
	code, body, _ := do(t, h, "GET", "/api/v1/openapi.json", "")
	if code != 200 {
		t.Fatalf("openapi.json = %d", code)
	}
	version, _ := body["openapi"].(string)
	if !strings.HasPrefix(version, "3.") {
		t.Errorf("openapi version = %q", version)
	}

	// Docs page references the local spec.
	req = httptest.NewRequest("GET", "/docs", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	html := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("docs = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(html, "api-reference") || !strings.Contains(html, "/api/v1/openapi.yaml") {
		t.Error("docs page does not wire the Scalar UI to the local spec")
	}
}
