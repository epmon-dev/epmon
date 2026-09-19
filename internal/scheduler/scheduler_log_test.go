package scheduler

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/store"
)

// TestDownLogger is a deterministic table test over the throttle policy
// using an injected clock: transitions always log, repeats at most
// once a minute with the count, recovery names the streak.
func TestDownLogger(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	l := &downLogger{now: func() time.Time { return now }}

	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	lines := func() int { return strings.Count(buf.String(), "\n") }

	l.report("web", true, "")
	if lines() != 0 {
		t.Fatalf("initial up logged %d lines, want 0", lines())
	}
	l.report("web", false, "status: got 500")
	if lines() != 1 || !strings.Contains(buf.String(), "web DOWN (status: got 500)") {
		t.Fatalf("transition logged %q, want one DOWN line", buf.String())
	}
	// Repeats within the minute stay silent but keep counting.
	for i := 0; i < 30; i++ {
		l.report("web", false, "status: got 500")
	}
	if lines() != 1 {
		t.Fatalf("repeats logged %d lines, want still 1", lines())
	}
	// First repeat after a minute logs once with the running count.
	now = now.Add(61 * time.Second)
	l.report("web", false, "status: got 500")
	if lines() != 2 || !strings.Contains(buf.String(), "still DOWN (32 consecutive failures") {
		t.Fatalf("minute repeat logged %q, want still-DOWN with count 32", buf.String())
	}
	// Recovery names the streak and resets.
	l.report("web", true, "")
	if lines() != 3 || !strings.Contains(buf.String(), "web UP (recovered after 32 failed probes)") {
		t.Fatalf("recovery logged %q, want UP with count 32", buf.String())
	}
}

// logStore counts RecordCheck calls; logObserver is a no-op.
type logStore struct {
	mu sync.Mutex
	n  int
}

func (s *logStore) RecordCheck(context.Context, store.Check) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return nil
}

func (s *logStore) Purge(context.Context, int, time.Time) (int64, error) { return 0, nil }

type logObserver struct{}

func (logObserver) ObserveCheck(string, bool, int64)               {}
func (logObserver) ObserveProbe(string, bool, bool, time.Duration) {}
func (logObserver) ObserveSkipped(string)                          {}

// TestDownLoggerWiring runs a failing service at 10ms intervals and
// asserts the loop emits one transition line instead of one per probe.
// Recovery is covered deterministically by TestDownLogger above: asserting
// it here would race a context-cancelled in-flight probe at shutdown,
// which legitimately reports down (up→down transition).
func TestDownLoggerWiring(t *testing.T) {
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
				ID: "web", Name: "Web", URL: target.URL, Method: "GET",
				Interval:     config.Duration(10 * time.Millisecond),
				Timeout:      config.Duration(2 * time.Second),
				ExpectStatus: spec,
			},
		},
	}
	s := New(cfg, &logStore{}, logObserver{})

	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	s.Run(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()
	s.Stop()

	// Read the buffer only after Stop: the loop goroutine is gone, so no
	// read/write race on the builder. A probe cancelled by shutdown stays
	// down→down and is silent, so the count is deterministic.
	downLines := strings.Count(buf.String(), "web DOWN (")
	stillLines := strings.Count(buf.String(), "still DOWN")
	if downLines != 1 {
		t.Errorf("got %d DOWN transition lines, want 1 (log volume not damped)", downLines)
	}
	if stillLines != 0 {
		t.Errorf("got %d repeat lines, want 0", stillLines)
	}
}
