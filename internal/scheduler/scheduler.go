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

// Run starts one loop per service plus the daily purge. It returns immediately.
func (s *Scheduler) Run(ctx context.Context) {
	n := s.cfg.Probes.Concurrency
	if n <= 0 {
		n = 64
	}
	// sem bounds concurrent probe executions across all loops so fleet
	// size never sets the outbound concurrency by itself.
	sem := make(chan struct{}, n)
	for _, svc := range s.cfg.Services {
		s.wg.Add(1)
		go s.loop(ctx, svc, sem)
	}
	s.wg.Add(1)
	go s.purgeLoop(ctx)
}

// Stop waits for every loop to exit.
func (s *Scheduler) Stop() { s.wg.Wait() }

func (s *Scheduler) loop(ctx context.Context, svc config.Service, sem chan struct{}) {
	defer s.wg.Done()
	probe := func() {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
		check := prober.Probe(ctx, svc)
		if err := s.checks.RecordCheck(ctx, check); err != nil {
			log.Printf("epmon: record %s: %v", svc.ID, err)
			return
		}
		s.observe.ObserveCheck(svc.ID, check.Up, check.LatencyMs)
		if !check.Up {
			log.Printf("epmon: %s DOWN (%s)", svc.ID, check.Error)
		}
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
