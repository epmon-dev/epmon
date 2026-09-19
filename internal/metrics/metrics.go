// Package metrics exposes probe outcomes and process state in Prometheus
// text format 0.0.4.
//
// One Registry is shared between the scheduler (records), the store actor
// (queue/counter hooks via the Metrics interface) and the HTTP layer
// (serves). All methods are goroutine-safe. Metric names and labels are
// compatibility surface (§15): renames are breaking.
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Histogram buckets for epmon_probe_duration_seconds.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Observer is the write side the scheduler needs.
type Observer interface {
	ObserveCheck(serviceID string, up bool, latencyMs int64)
	ObserveProbe(serviceID string, rawUp, stateUp bool, latency time.Duration)
	ObserveSkipped(serviceID string)
}

type serviceStats struct {
	success, failure int64
	skipped          int64
	stateUp          bool
	known            bool
	// buckets counts samples per duration bucket (last slot is the +Inf
	// overflow); sum/count feed the histogram sum and count. Constant
	// size per service regardless of uptime — samples are never stored.
	buckets []uint64
	sum     float64
	count   uint64
}

// Registry accumulates the §10.1 catalog.
type Registry struct {
	mu       sync.Mutex
	services map[string]*serviceStats
	start    time.Time

	dropped      atomic.Uint64
	writeTimeout atomic.Uint64
	migratedOK   atomic.Uint64
	migratedMiss atomic.Uint64
	reloadOK     atomic.Uint64
	reloadErr    atomic.Uint64

	cmdDepth, checkDepth atomic.Int64

	version, commit string
}

// New builds an empty Registry.
func New() *Registry {
	return &Registry{services: map[string]*serviceStats{}, start: time.Now()}
}

// ObserveProbe records one probe: raw outcome counters, state gauge and
// header-latency histogram sample.
func (r *Registry) ObserveProbe(serviceID string, rawUp, stateUp bool, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.statLocked(serviceID)
	if rawUp {
		st.success++
	} else {
		st.failure++
	}
	st.stateUp = stateUp
	st.known = true
	secs := latency.Seconds()
	st.buckets[sort.SearchFloat64s(durationBuckets, secs)]++
	st.sum += secs
	st.count++
}

// ObserveCheck is the legacy write path: up flag plus integer milliseconds
// from store.Check.LatencyMs. It maps onto ObserveProbe with the up flag
// doubling as the state (single-sample state machine).
func (r *Registry) ObserveCheck(serviceID string, up bool, latencyMs int64) {
	r.ObserveProbe(serviceID, up, up, time.Duration(latencyMs)*time.Millisecond)
}

// ObserveSkipped counts an overlap-guard skip.
func (r *Registry) ObserveSkipped(serviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statLocked(serviceID).skipped++
}

func (r *Registry) statLocked(id string) *serviceStats {
	st, ok := r.services[id]
	if !ok {
		st = &serviceStats{buckets: make([]uint64, len(durationBuckets)+1)}
		r.services[id] = st
	}
	return st
}

// IncStoreDropped counts a checkCh overflow drop.
func (r *Registry) IncStoreDropped() { r.dropped.Add(1) }

// IncStoreWriteTimeout counts a command-budget expiry (503).
func (r *Registry) IncStoreWriteTimeout() { r.writeTimeout.Add(1) }

// IncMigrated counts alias-migration outcomes (ok|no_match).
func (r *Registry) IncMigrated(result string) {
	if result == "ok" {
		r.migratedOK.Add(1)
	} else {
		r.migratedMiss.Add(1)
	}
}

// IncReload counts config-reload outcomes (ok|error).
func (r *Registry) IncReload(result string) {
	if result == "ok" {
		r.reloadOK.Add(1)
	} else {
		r.reloadErr.Add(1)
	}
}

// SetQueueDepths records current actor queue lengths (gauges).
func (r *Registry) SetQueueDepths(cmd, check int) {
	r.cmdDepth.Store(int64(cmd))
	r.checkDepth.Store(int64(check))
}

// SetStoreTotals syncs cumulative store counters (monotonic per process).
func (r *Registry) SetStoreTotals(dropped, writeTimeout uint64) {
	for {
		cur := r.dropped.Load()
		if dropped <= cur || r.dropped.CompareAndSwap(cur, dropped) {
			break
		}
	}
	for {
		cur := r.writeTimeout.Load()
		if writeTimeout <= cur || r.writeTimeout.CompareAndSwap(cur, writeTimeout) {
			break
		}
	}
}

// SetBuildInfo stamps epmon_build_info.
func (r *Registry) SetBuildInfo(version, commit string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version, r.commit = version, commit
}

func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// Snapshot renders Prometheus text format 0.0.4.
func (r *Registry) Snapshot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.services))
	for id := range r.services {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("# HELP epmon_service_up 1 iff service state is UP.\n")
	b.WriteString("# TYPE epmon_service_up gauge\n")
	for _, id := range ids {
		v := 0
		if st := r.services[id]; st.known && st.stateUp {
			v = 1
		}
		fmt.Fprintf(&b, "epmon_service_up{service=\"%s\"} %d\n", escapeLabel(id), v)
	}
	b.WriteString("# HELP epmon_probe_total Raw probe outcomes.\n")
	b.WriteString("# TYPE epmon_probe_total counter\n")
	for _, id := range ids {
		st := r.services[id]
		fmt.Fprintf(&b, "epmon_probe_total{service=\"%s\",result=\"success\"} %d\n", escapeLabel(id), st.success)
		fmt.Fprintf(&b, "epmon_probe_total{service=\"%s\",result=\"failure\"} %d\n", escapeLabel(id), st.failure)
	}
	b.WriteString("# HELP epmon_probe_duration_seconds Header latency.\n")
	b.WriteString("# TYPE epmon_probe_duration_seconds histogram\n")
	for _, id := range ids {
		st := r.services[id]
		var cum uint64
		for i, le := range durationBuckets {
			cum += st.buckets[i]
			fmt.Fprintf(&b, "epmon_probe_duration_seconds_bucket{service=\"%s\",le=\"%g\"} %d\n", escapeLabel(id), le, cum)
		}
		cum += st.buckets[len(durationBuckets)]
		fmt.Fprintf(&b, "epmon_probe_duration_seconds_bucket{service=\"%s\",le=\"+Inf\"} %d\n", escapeLabel(id), cum)
		fmt.Fprintf(&b, "epmon_probe_duration_seconds_sum{service=\"%s\"} %g\n", escapeLabel(id), st.sum)
		fmt.Fprintf(&b, "epmon_probe_duration_seconds_count{service=\"%s\"} %d\n", escapeLabel(id), st.count)
	}
	b.WriteString("# HELP epmon_probe_skipped_total Overlap-guard skips.\n")
	b.WriteString("# TYPE epmon_probe_skipped_total counter\n")
	for _, id := range ids {
		fmt.Fprintf(&b, "epmon_probe_skipped_total{service=\"%s\"} %d\n", escapeLabel(id), r.services[id].skipped)
	}
	fmt.Fprintf(&b, "# HELP epmon_store_queue_depth Pending PutCheck batches.\n# TYPE epmon_store_queue_depth gauge\nepmon_store_queue_depth %d\n", r.checkDepth.Load())
	fmt.Fprintf(&b, "# HELP epmon_store_cmd_queue_depth Pending commands.\n# TYPE epmon_store_cmd_queue_depth gauge\nepmon_store_cmd_queue_depth %d\n", r.cmdDepth.Load())
	fmt.Fprintf(&b, "# HELP epmon_store_dropped_total Overflow-dropped checks.\n# TYPE epmon_store_dropped_total counter\nepmon_store_dropped_total %d\n", r.dropped.Load())
	fmt.Fprintf(&b, "# HELP epmon_store_write_timeout_total Aborted API writes.\n# TYPE epmon_store_write_timeout_total counter\nepmon_store_write_timeout_total %d\n", r.writeTimeout.Load())
	b.WriteString("# HELP epmon_service_history_migrated_total Alias migrations.\n# TYPE epmon_service_history_migrated_total counter\n")
	fmt.Fprintf(&b, "epmon_service_history_migrated_total{result=\"ok\"} %d\n", r.migratedOK.Load())
	fmt.Fprintf(&b, "epmon_service_history_migrated_total{result=\"no_match\"} %d\n", r.migratedMiss.Load())
	b.WriteString("# HELP epmon_config_reload_total Config reloads.\n# TYPE epmon_config_reload_total counter\n")
	fmt.Fprintf(&b, "epmon_config_reload_total{result=\"ok\"} %d\n", r.reloadOK.Load())
	fmt.Fprintf(&b, "epmon_config_reload_total{result=\"error\"} %d\n", r.reloadErr.Load())
	fmt.Fprintf(&b, "# HELP epmon_uptime_seconds Process uptime.\n# TYPE epmon_uptime_seconds gauge\nepmon_uptime_seconds %d\n", int64(time.Since(r.start)/time.Second))
	fmt.Fprintf(&b, "# HELP epmon_build_info Build metadata.\n# TYPE epmon_build_info gauge\nepmon_build_info{version=\"%s\",commit=\"%s\"} 1\n",
		escapeLabel(r.version), escapeLabel(r.commit))
	return b.String()
}

// Handler serves the registry at GET /metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Snapshot()))
	})
}
