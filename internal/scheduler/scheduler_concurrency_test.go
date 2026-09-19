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

type concurrencyRecorder struct {
	mu sync.Mutex
	n  int
}

func (r *concurrencyRecorder) RecordCheck(context.Context, store.Check) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	return nil
}

func (r *concurrencyRecorder) Purge(context.Context, int, time.Time) (int64, error) {
	return 0, nil
}

type concurrencyObserver struct{}

func (concurrencyObserver) ObserveCheck(string, bool, int64)               {}
func (concurrencyObserver) ObserveProbe(string, bool, bool, time.Duration) {}
func (concurrencyObserver) ObserveSkipped(string)                          {}

// TestSchedulerBoundsProbeConcurrency parks up to 5 service loops inside a
// blocking target and asserts no more than probes.concurrency=2 are ever
// inside a probe at once. The bound holds regardless of timing; the test
// only waits until contention is proven (active >= 3 attempted).
func TestSchedulerBoundsProbeConcurrency(t *testing.T) {
	var active, maxSeen atomic.Int64
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := active.Add(1)
		for {
			m := maxSeen.Load()
			if cur <= m || maxSeen.CompareAndSwap(m, cur) {
				break
			}
		}
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		active.Add(-1)
	}))
	defer target.Close()

	spec := config.StatusSpec{}
	if err := spec.UnmarshalJSON([]byte("[200]")); err != nil {
		t.Fatal(err)
	}
	services := make([]config.Service, 0, 5)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		services = append(services, config.Service{
			ID: id, Name: id, URL: target.URL, Method: "GET",
			Interval:     config.Duration(10 * time.Millisecond),
			Timeout:      config.Duration(5 * time.Second),
			ExpectStatus: spec,
		})
	}
	cfg := &config.Config{
		Services: services,
		Probes:   config.Probes{Concurrency: 2},
	}
	s := New(cfg, &concurrencyRecorder{}, concurrencyObserver{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for active.Load() < 2 && maxSeen.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give the remaining loops a chance to pile onto the semaphore.
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(release)
	s.Stop()

	if got := maxSeen.Load(); got < 2 {
		t.Fatalf("max concurrent probes = %d, contention never materialized", got)
	}
	if got := maxSeen.Load(); got > 2 {
		t.Errorf("max concurrent probes = %d, want <= probes.concurrency=2", got)
	}
}
