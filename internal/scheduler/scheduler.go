// Package scheduler probes every configured service on its own interval
// and purges expired checks once a day. One goroutine per service;
// Stop blocks until all loops exit.
package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/metrics"
	"github.com/epmon-dev/epmon/internal/prober"
	"github.com/epmon-dev/epmon/internal/store"
)

// Scheduler owns the probe loops. It writes through CheckRecorder only —
// it cannot read, migrate, or otherwise touch storage. Every result is
// also reported to the metrics observer for /metrics.
type Scheduler struct {
	cfg     *config.Config
	checks  store.CheckRecorder
	observe metrics.Observer
	wg      sync.WaitGroup
}

// New wires a scheduler. Call Run to start, Stop to shut down.
func New(cfg *config.Config, checks store.CheckRecorder, observe metrics.Observer) *Scheduler {
	return &Scheduler{cfg: cfg, checks: checks, observe: observe}
}

// Run starts one loop per enabled service plus the daily purge. It returns immediately.
func (s *Scheduler) Run(ctx context.Context) {
	for _, svc := range s.cfg.Services {
		if !svc.EnabledOrDefault() {
			continue
		}
		s.wg.Add(1)
		go s.loop(ctx, svc)
	}
	s.wg.Add(1)
	go s.purgeLoop(ctx)
}

// Stop waits for every loop to exit.
func (s *Scheduler) Stop() { s.wg.Wait() }

func (s *Scheduler) loop(ctx context.Context, svc config.Service) {
	defer s.wg.Done()
	logger := newDownLogger()
	// Consecutive-failure counting for failure_threshold: the raw probe is
	// always stored, but the reported state flips to down only after
	// threshold breaches in a row; any success resets the counter.
	threshold := svc.FailureThreshold
	if threshold < 1 {
		threshold = 1
	}
	consecutive := 0
	probe := func() {
		check := prober.Probe(ctx, svc)
		if err := s.checks.RecordCheck(ctx, check); err != nil {
			log.Printf("epmon: record %s: %v", svc.ID, err)
			return
		}
		stateUp := true
		if !check.Up {
			consecutive++
			stateUp = consecutive < threshold
		} else {
			consecutive = 0
		}
		s.observe.ObserveProbe(svc.ID, check.Up, stateUp, time.Duration(check.LatencyMs)*time.Millisecond)
		logger.report(svc.ID, stateUp, check.Error)
	}
	probe()
	ticker := time.NewTicker(svc.Interval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe()
		}
	}
}

// downLogger keeps per-service failure logging informative at any probe
// rate: transitions (up→down, down→up) always log, while a sustained
// outage re-logs at most once a minute with the running count included.
type downLogger struct {
	known, lastUp bool
	downs         int
	lastLog       time.Time
	now           func() time.Time
}

func newDownLogger() *downLogger { return &downLogger{now: time.Now} }

func (l *downLogger) report(id string, up bool, errMsg string) {
	now := l.now()
	if !l.known || up != l.lastUp {
		if !up {
			l.downs = 1
			log.Printf("epmon: %s DOWN (%s)", id, errMsg)
		} else if l.known && l.downs > 0 {
			log.Printf("epmon: %s UP (recovered after %d failed probes)", id, l.downs)
			l.downs = 0
		}
		l.known, l.lastUp, l.lastLog = true, up, now
		return
	}
	if !up {
		l.downs++
		if now.Sub(l.lastLog) >= time.Minute {
			log.Printf("epmon: %s still DOWN (%d consecutive failures, last: %s)", id, l.downs, errMsg)
			l.lastLog = now
		}
	}
}

func (s *Scheduler) purgeLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.checks.Purge(ctx, s.cfg.Database.RetentionDays, time.Now())
			if err != nil {
				log.Printf("epmon: purge: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("epmon: purged %d expired checks", n)
			}
		}
	}
}
