package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/metrics"
	"github.com/epmon-dev/epmon/internal/store"
)

type countRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *countRecorder) RecordCheck(_ context.Context, c store.Check) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts == nil {
		r.counts = map[string]int{}
	}
	r.counts[c.ServiceID]++
	return nil
}

func (r *countRecorder) Purge(_ context.Context, _ int, _ time.Time) (int64, error) {
	return 0, nil
}

func (r *countRecorder) n(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[id]
}

type noopObserver struct{}

func (noopObserver) ObserveCheck(string, bool, int64)               {}
func (noopObserver) ObserveProbe(string, bool, bool, time.Duration) {}
func (noopObserver) ObserveSkipped(string)                          {}

var _ metrics.Observer = noopObserver{}

// TestDisabledServicesNotProbed starts one enabled and one disabled service
// and asserts the disabled one is never probed while the enabled one is.
func TestDisabledServicesNotProbed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer target.Close()

	off := false
	spec := config.StatusSpec{}
	if err := spec.UnmarshalJSON([]byte("[200]")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Services: []config.Service{
			{
				ID: "on", Name: "On", URL: target.URL, Method: "GET",
				Interval:     config.Duration(10 * time.Millisecond),
				Timeout:      config.Duration(2 * time.Second),
				ExpectStatus: spec,
			},
			{
				ID: "off", Name: "Off", URL: target.URL, Method: "GET",
				Interval: config.Duration(10 * time.Millisecond),
				Timeout:  config.Duration(2 * time.Second),
				Enabled:  &off,
			},
		},
	}
	rec := &countRecorder{}
	s := New(cfg, rec, noopObserver{})

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()
	s.Stop()

	if got := rec.n("on"); got < 1 {
		t.Errorf("enabled service probed %d times, want >= 1", got)
	}
	if got := rec.n("off"); got != 0 {
		t.Errorf("disabled service probed %d times, want 0", got)
	}
}
