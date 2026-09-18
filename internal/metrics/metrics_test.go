package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSnapshot(t *testing.T) {
	r := New()
	r.ObserveCheck("web", true, 42)
	r.ObserveCheck("web", false, 7)
	r.ObserveCheck(`odd"name`, true, 1)

	out := r.Snapshot()
	for _, want := range []string{
		`epmon_service_up{service="web"} 0`,
		`epmon_probe_total{service="web",result="success"} 1`,
		`epmon_probe_total{service="web",result="failure"} 1`,
		`epmon_probe_duration_seconds_bucket{service="web",le="0.05"} 2`,
		`epmon_probe_duration_seconds_count{service="web"} 2`,
		`epmon_service_up{service="odd\"name"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q\n%s", want, out)
		}
	}
}

func TestHandler(t *testing.T) {
	r := New()
	h := r.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
}
