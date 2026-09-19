package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/store"
)

// suiteRecorder is a fake CheckRecorder counting checks and capturing the
// last purge call.
type suiteRecorder struct {
	mu         sync.Mutex
	checks     int
	purgeDays  int
	purgeCalls int
	purgeErr   error
	entered    chan struct{}
	release    chan struct{}
	blocking   bool
}

func (r *suiteRecorder) RecordCheck(context.Context, store.Check) error {
	if r.blocking {
		select {
		case <-r.entered:
		default:
			close(r.entered)
		}
		<-r.release
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	return nil
}

func (r *suiteRecorder) Purge(_ context.Context, retentionDays int, _ time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgeDays = retentionDays
	r.purgeCalls++
	return 0, r.purgeErr
}

func (r *suiteRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks
}

type suiteObserver struct{}

func (suiteObserver) ObserveCheck(string, bool, int64)               {}
func (suiteObserver) ObserveProbe(string, bool, bool, time.Duration) {}
func (suiteObserver) ObserveSkipped(string)                          {}

func suiteService(url string, interval config.Duration) config.Service {
	spec := config.StatusSpec{}
	_ = spec.UnmarshalJSON([]byte("[200]"))
	return config.Service{
		ID: "web", Name: "Web", URL: url, Method: "GET",
		Interval:     interval,
		Timeout:      config.Duration(2 * time.Second),
		ExpectStatus: spec,
	}
}

// TestSchedulerProbesImmediately asserts the first probe fires at startup,
// not after one interval: with a 1h period, a short run records exactly 1.
func TestSchedulerProbesImmediately(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	cfg := &config.Config{Services: []config.Service{suiteService(target.URL, config.Duration(time.Hour))}}
	rec := &suiteRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(cfg, rec, suiteObserver{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	s.Stop()

	if got := rec.count(); got != 1 {
		t.Errorf("recorded %d probes with 1h interval in 50ms, want exactly 1", got)
	}
}

// TestSchedulerTicksAtInterval asserts steady ticks at the configured
// period: immediate + ~20ms + ~40ms land at least 3 checks in 70ms.
func TestSchedulerTicksAtInterval(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	cfg := &config.Config{Services: []config.Service{suiteService(target.URL, config.Duration(20*time.Millisecond))}}
	rec := &suiteRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(cfg, rec, suiteObserver{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(70 * time.Millisecond)
	cancel()
	s.Stop()

	if got := rec.count(); got < 3 {
		t.Errorf("recorded %d probes in 70ms at 20ms interval, want >= 3", got)
	}
}

// TestSchedulerPurgePassesRetention asserts purges carry the configured
// retention window and that a failing purge doesn't propagate.
func TestSchedulerPurgePassesRetention(t *testing.T) {
	cfg := &config.Config{Database: config.Database{RetentionDays: 45}}
	rec := &suiteRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	s := New(cfg, rec, suiteObserver{})

	s.purgeOnce(context.Background())
	if rec.purgeDays != 45 || rec.purgeCalls != 1 {
		t.Errorf("purge got days=%d calls=%d, want 45/1", rec.purgeDays, rec.purgeCalls)
	}

	rec.purgeErr = errors.New("locked")
	s.purgeOnce(context.Background()) // must not panic or return
	if rec.purgeCalls != 2 {
		t.Errorf("purge calls = %d, want 2", rec.purgeCalls)
	}
}

// TestSchedulerStopWaitsForInflight starts a run, parks a probe inside a
// blocking RecordCheck, cancels, and asserts Stop returns only after the
// parked probe completes.
func TestSchedulerStopWaitsForInflight(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	cfg := &config.Config{Services: []config.Service{suiteService(target.URL, config.Duration(time.Hour))}}
	rec := &suiteRecorder{entered: make(chan struct{}), release: make(chan struct{}), blocking: true}
	s := New(cfg, rec, suiteObserver{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	select {
	case <-rec.entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("probe never started")
	}
	cancel()

	stopped := make(chan struct{})
	go func() {
		s.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a probe was still parked")
	case <-time.After(100 * time.Millisecond):
	}
	close(rec.release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the probe completed")
	}
	if got := rec.count(); got != 1 {
		t.Errorf("recorded %d probes, want 1", got)
	}
}
