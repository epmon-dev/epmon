package api

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
)

func serverWith(t *testing.T, mutate func(*config.Server)) *Server {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{
			RateLimitRPM:   60,
			RateLimitBurst: 60,
			MaxBodyBytes:   1 << 20,
		},
		Services: []config.Service{
			{ID: "web", Name: "Web", URL: "https://example.com"},
		},
	}
	mutate(&cfg.Server)
	return New(cfg, openTestStore(t), time.Now)
}

func TestRateLimit(t *testing.T) {
	srv := serverWith(t, func(s *config.Server) {
		s.RateLimitRPM = 60
		s.RateLimitBurst = 1
	})
	h := srv.Handler()

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest("GET", "/api/v1/status", nil))
	if first.Code != 200 {
		t.Fatalf("first = %d", first.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("GET", "/api/v1/status", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}
}

func TestWriteAuth(t *testing.T) {
	srv := serverWith(t, func(s *config.Server) {
		s.APIKeys = []string{"s3cret"}
	})
	h := srv.Handler()

	// Reads stay public.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if rec.Code != 200 {
		t.Fatalf("public read = %d", rec.Code)
	}
	// Writes without and with wrong keys fail.
	for _, key := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest("POST", "/api/v1/incidents", strings.NewReader(`{"title":"x"}`))
		if key != "" {
			req.Header.Set("Authorization", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("key %q = %d, want 401", key, rec.Code)
		}
	}
	// Right key passes.
	req := httptest.NewRequest("POST", "/api/v1/incidents", strings.NewReader(`{"title":"x"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Errorf("authorized write = %d, want 201", rec.Code)
	}
}

func TestBodyCap(t *testing.T) {
	srv := serverWith(t, func(s *config.Server) {
		s.MaxBodyBytes = 16
	})
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/api/v1/incidents", strings.NewReader(`{"title":"way too long for 16 bytes"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d, want 413", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	srv := serverWith(t, func(s *config.Server) {
		s.CORSAllowedOrigins = []string{"https://app.example.com"}
	})
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin = %q", got)
	}

	req = httptest.NewRequest("OPTIONS", "/api/v1/incidents", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight = %d, want 204", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/v1/status", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin got Allow-Origin = %q", got)
	}
}

func TestSecurityHeadersAndRecovery(t *testing.T) {
	srv := serverWith(t, func(*config.Server) {})
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}

	panicker := recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec = httptest.NewRecorder()
	panicker.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 500 {
		t.Errorf("panic = %d, want 500", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 70.0.0.1")
	if got := clientIP(false, req); got != "10.0.0.1" {
		t.Errorf("untrusted XFF = %q", got)
	}
	if got := clientIP(true, req); got != "203.0.113.9" {
		t.Errorf("trusted XFF = %q", got)
	}
}

func TestHealthzReadiness(t *testing.T) {
	srv, st := testServer(t)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthy = %d", rec.Code)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("dead store = %d, want 503", rec.Code)
	}
}

// TestLogRecordsOutcome asserts the access log carries the response status
// on both success and failure paths (a closed store forces the 500).
func TestLogRecordsOutcome(t *testing.T) {
	srv, st := testServer(t)
	h := Log(srv.Handler())

	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("dead store status = %d, want 500", rec.Code)
	}

	out := buf.String()
	if !strings.Contains(out, "GET /api/v1/status 200 ") {
		t.Errorf("log missing 200 line, got %q", out)
	}
	if !strings.Contains(out, "GET /api/v1/status 500 ") {
		t.Errorf("log missing 500 line, got %q", out)
	}
}
