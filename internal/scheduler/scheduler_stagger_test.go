package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/store"
)

type staggerRecorder struct {
	mu sync.Mutex
	n  int
}

func (r *staggerRecorder) RecordCheck(context.Context, store.Check) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil
}

func (r *staggerRecorder) Purge(context.Context, int, time.Time) (int64, error) {
	return 0, nil
}

func (r *staggerRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

type staggerObserver struct{}

func (staggerObserver) ObserveCheck(string, bool, int64)               {}
func (staggerObserver) ObserveProbe(string, bool, bool, time.Duration) {}
func (staggerObserver) ObserveSkipped(string)                          {}

func staggerService(url string) config.Service {
	spec := config.StatusSpec{}
	_ = spec.UnmarshalJSON([]byte("[200]"))
	return config.Service{
		ID: "web", Name: "Web", URL: url, Method: "GET",
		Interval:     config.Duration(20 * time.Millisecond),
		Timeout:      config.Duration(2 * time.Second),
		ExpectStatus: spec,
	}
}

// TestSchedulerStagger pins the phase to 200ms and runs 60ms: only the
// immediate probe may have fired.
func TestSchedulerStagger(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	cfg := &config.Config{Services: []config.Service{staggerService(target.URL)}}
	rec := &staggerRecorder{}
	s := New(cfg, rec, staggerObserver{})
	s.phase = func(time.Duration) time.Duration { return 200 * time.Millisecond }

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()
	s.Stop()

	if got := rec.count(); got != 1 {
		t.Errorf("recorded %d probes in 60ms with 200ms phase, want exactly 1", got)
	}
}

// TestSchedulerNoStagger pins the phase to zero: ticks proceed at the
// configured period (immediate + ~20ms + ~40ms within 60ms).
func TestSchedulerNoStagger(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	cfg := &config.Config{Services: []config.Service{staggerService(target.URL)}}
	rec := &staggerRecorder{}
	s := New(cfg, rec, staggerObserver{})
	s.phase = func(time.Duration) time.Duration { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()
	s.Stop()

	if got := rec.count(); got < 3 {
		t.Errorf("recorded %d probes in 60ms with zero phase, want >= 3", got)
	}
}

// spreadRecorder captures per-service probe timestamps.
type spreadRecorder struct {
	mu sync.Mutex
	ts map[string][]time.Duration
	t0 time.Time
}

func (r *spreadRecorder) RecordCheck(_ context.Context, c store.Check) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ts[c.ServiceID] = append(r.ts[c.ServiceID], time.Since(r.t0))
	return nil
}

func (r *spreadRecorder) Purge(context.Context, int, time.Time) (int64, error) {
	return 0, nil
}

func (r *spreadRecorder) stamps(id string) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.ts[id]...)
}

// TestSchedulerStaggerSeparates runs two same-interval services with
// in-range phases 5ms vs 45ms and asserts their steady ticks stay ~40ms
// apart instead of aligned. (The pre-fix code anchored ticks to loop
// start, yielding a ~0 gap.)
func TestSchedulerStaggerSeparates(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	spec := config.StatusSpec{}
	if err := spec.UnmarshalJSON([]byte("[200]")); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) config.Service {
		return config.Service{
			ID: id, Name: id, URL: target.URL, Method: "GET",
			Interval:     config.Duration(50 * time.Millisecond),
			Timeout:      config.Duration(2 * time.Second),
			ExpectStatus: spec,
		}
	}
	cfg := &config.Config{Services: []config.Service{mk("a"), mk("b")}}
	rec := &spreadRecorder{ts: map[string][]time.Duration{}, t0: time.Now()}
	s := New(cfg, rec, staggerObserver{})
	// Loop start order across goroutines is unspecified, so hand out the
	// two phases atomically without assuming which service is first: the
	// assertion below uses the absolute gap.
	var calls atomic.Int64
	s.phase = func(time.Duration) time.Duration {
		if calls.Add(1) == 1 {
			return 5 * time.Millisecond
		}
		return 45 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(160 * time.Millisecond)
	cancel()
	s.Stop()

	a, b := rec.stamps("a"), rec.stamps("b")
	if len(a) < 2 || len(b) < 2 {
		t.Fatalf("too few probes: a=%d b=%d", len(a), len(b))
	}
	if gap := absDuration(b[1] - a[1]); gap < 20*time.Millisecond {
		t.Errorf("second-tick gap = %v, want ~40ms (ticks must not align)", gap)
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// TestRandomPhaseBounds pins the production phase source to [0, interval).
func TestRandomPhaseBounds(t *testing.T) {
	iv := 60 * time.Second
	for i := 0; i < 200; i++ {
		if got := randomPhase(iv); got < 0 || got >= iv {
			t.Fatalf("randomPhase(%v) = %v, want [0, interval)", iv, got)
		}
	}
	if got := randomPhase(0); got != 0 {
		t.Errorf("randomPhase(0) = %v, want 0", got)
	}
}
