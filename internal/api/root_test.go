package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
)

// TestRootGating pins the spec §9 contract: the SPA shell when enabled,
// the historical JSON 404 when disabled, and JSON 404 for /api/*
// unknowns in both modes (API clients must never get HTML).
func TestRootGating(t *testing.T) {
	srv, _ := testServer(t) // StatusPage.Enabled nil → default true
	enabled := srv.Handler()

	falsy := false
	offCfg := &config.Config{
		Server: config.Server{
			MaxBodyBytes: 1 << 20,
			StatusPage:   config.StatusPageConfig{Enabled: &falsy},
		},
		Services: []config.Service{
			{ID: "web", Name: "Web", URL: "https://example.com"},
		},
	}
	off := New(offCfg, openTestStore(t), time.Now).Handler()

	serve := func(h http.Handler, path string) (int, string, http.Header) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), rec.Header()
	}

	if code, _, header := serve(enabled, "/"); code != 302 || header.Get("Location") != "/local" {
		t.Errorf("enabled / = %d, want redirect to /local", code)
	}
	if code, body, _ := serve(enabled, "/local"); code != 200 || !strings.Contains(body, `<div id="root">`) {
		t.Errorf("enabled /local = %d, want the app shell", code)
	}
	if code, body, _ := serve(enabled, "/p/acme"); code != 200 || !strings.Contains(body, `<div id="root">`) {
		t.Errorf("enabled /p/acme = %d, want SPA fallback", code)
	}
	if code, body, _ := serve(enabled, "/api/v1/nope"); code != 404 || !strings.Contains(body, "not_found") {
		t.Errorf("enabled /api/v1/nope = %d %q, want JSON 404", code, body)
	}
	if code, body, _ := serve(off, "/"); code != 404 || !strings.Contains(body, "not_found") {
		t.Errorf("disabled / = %d %q, want JSON 404", code, body)
	}
	if code, body, _ := serve(off, "/api/v1/nope"); code != 404 || !strings.Contains(body, "not_found") {
		t.Errorf("disabled /api/v1/nope = %d %q, want JSON 404", code, body)
	}
}
