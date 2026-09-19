package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestHistogramBuckets pins cumulative bucket boundaries: 5ms lands in
// le=0.005, 50ms in le=0.05, 5s in le=5.
func TestHistogramBuckets(t *testing.T) {
	r := New()
	r.ObserveProbe("web", true, true, 5*time.Millisecond)
	r.ObserveProbe("web", true, true, 50*time.Millisecond)
	r.ObserveProbe("web", true, true, 5*time.Second)

	out := r.Snapshot()
	for _, want := range []string{
		`epmon_probe_duration_seconds_bucket{service="web",le="0.005"} 1`,
		`epmon_probe_duration_seconds_bucket{service="web",le="0.05"} 2`,
		`epmon_probe_duration_seconds_bucket{service="web",le="5"} 3`,
		`epmon_probe_duration_seconds_bucket{service="web",le="+Inf"} 3`,
		`epmon_probe_duration_seconds_count{service="web"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q\n%s", want, out)
		}
	}
}

// TestHistogramBoundedState proves per-service histogram state is constant
// size no matter how many samples flow through it.
func TestHistogramBoundedState(t *testing.T) {
	r := New()
	const n = 200000
	for i := 0; i < n; i++ {
		r.ObserveProbe("web", true, true, time.Duration(i%1000)*time.Millisecond)
	}
	st := r.services["web"]
	if len(st.buckets) != len(durationBuckets)+1 {
		t.Fatalf("buckets = %d slots, want %d", len(st.buckets), len(durationBuckets)+1)
	}
	if st.count != n {
		t.Errorf("count = %d, want %d", st.count, n)
	}
	out := r.Snapshot()
	if want := `epmon_probe_duration_seconds_count{service="web"} 200000`; !strings.Contains(out, want) {
		t.Errorf("snapshot missing %q", want)
	}
}

// TestBuildInfo pins the stamped build identity line. The binary wires
// its ldflags version/commit into the registry at boot (see runDefault);
// an empty rendering means that wiring regressed.
func TestBuildInfo(t *testing.T) {
	r := New()
	r.SetBuildInfo("v1.2.3", "abc1234")
	out := r.Snapshot()
	want := `epmon_build_info{version="v1.2.3",commit="abc1234"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("snapshot missing %q\n%s", want, out)
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
