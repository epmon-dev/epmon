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

type thresholdObservation struct {
	rawUp, stateUp bool
}

// thresholdObserver captures every ObserveProbe call in order.
type thresholdObserver struct {
	mu   sync.Mutex
	obs  []thresholdObservation
	seen int
}

func (o *thresholdObserver) ObserveCheck(string, bool, int64) {}
func (o *thresholdObserver) ObserveProbe(_ string, rawUp, stateUp bool, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.obs = append(o.obs, thresholdObservation{rawUp, stateUp})
}
func (o *thresholdObserver) ObserveSkipped(string) {}

func (o *thresholdObserver) prefix(n int) []thresholdObservation {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.obs) < n {
		return nil
	}
	return append([]thresholdObservation(nil), o.obs[:n]...)
}

type thresholdStore struct {
	mu     sync.Mutex
	checks int
}

func (s *thresholdStore) RecordCheck(context.Context, store.Check) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	return nil
}

func (s *thresholdStore) Purge(context.Context, int, time.Time) (int64, error) {
	return 0, nil
}

// TestFailureThresholdDampsFlaps runs threshold=3 against a scripted
// target: requests 1-2 fail (below threshold, still reported up),
// request 3 succeeds (counter resets), requests 4-6 fail (third
// consecutive breach reports down).
func TestFailureThresholdDampsFlaps(t *testing.T) {
	var n atomic.Int64
	fail := map[int64]bool{1: true, 2: true, 4: true, 5: true, 6: true}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail[n.Add(1)] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}))
	defer target.Close()

	spec := config.StatusSpec{}
	if err := spec.UnmarshalJSON([]byte("[200]")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Services: []config.Service{
			{
				ID: "flap", Name: "Flap", URL: target.URL, Method: "GET",
				Interval:     config.Duration(10 * time.Millisecond),
				Timeout:      config.Duration(2 * time.Second),
				ExpectStatus: spec, FailureThreshold: 3,
			},
		},
	}
	obs := &thresholdObserver{}
	s := New(cfg, &thresholdStore{}, obs)

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()
	s.Stop()

	want := []thresholdObservation{
		{false, true}, {false, true}, {true, true},
		{false, true}, {false, true}, {false, false},
	}
	got := obs.prefix(len(want))
	if len(got) < len(want) {
		t.Fatalf("only %d observations, want at least %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("observation %d = %+v, want %+v (full prefix %+v)", i, got[i], want[i], got)
		}
	}
}

// TestFailureThresholdOne preserves today's behavior: the first failed
// probe reports down immediately.
func TestFailureThresholdOne(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	spec := config.StatusSpec{}
	if err := spec.UnmarshalJSON([]byte("[200]")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Services: []config.Service{
			{
				ID: "down", Name: "Down", URL: target.URL, Method: "GET",
				Interval:     config.Duration(10 * time.Millisecond),
				Timeout:      config.Duration(2 * time.Second),
				ExpectStatus: spec, FailureThreshold: 1,
			},
		},
	}
	obs := &thresholdObserver{}
	s := New(cfg, &thresholdStore{}, obs)

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	s.Stop()

	got := obs.prefix(1)
	if len(got) < 1 {
		t.Fatal("no observations recorded")
	}
	if got[0] != (thresholdObservation{false, false}) {
		t.Errorf("first observation = %+v, want {false false}", got[0])
	}
}
