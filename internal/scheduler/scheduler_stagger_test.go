package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
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
