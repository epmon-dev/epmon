// Command epmon monitors HTTP endpoints from a YAML/JSON catalogue,
// stores every probe in SQLite, and serves the results as JSON.
//
//	Usage: epmon [run|validate|healthcheck|version] [-config config.yaml]
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/epmon-dev/epmon/internal/api"
	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/metrics"
	"github.com/epmon-dev/epmon/internal/scheduler"
	"github.com/epmon-dev/epmon/internal/store"
	// Side-effect import: registers the "sqlite" driver with the store
	// registry. main never names the adapter type — cfg.Database.Driver
	// picks it. Same pattern as database/sql drivers.
	_ "github.com/epmon-dev/epmon/internal/store/sqlite"
)

const (
	exitOK      = 0
	exitConfig  = 1
	exitStorage = 2
	exitListen  = 3
	exitUnavail = 4
	exitUsage   = 64
)

// execute dispatches a subcommand; legacy flag-first invocations
// (epmon -config x.yaml) route to run. Exit codes: 0 ok, 1 config,
// 2 storage, 3 listen/bind, 4 healthcheck failure, 64 usage.
func execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (len(args) > 0 && strings.HasPrefix(args[0], "-")) {
		// Default: run with -config flag (or no args).
		return runDefault(args, stdout, stderr)
	}
	switch args[0] {
	case "run":
		return runDefault(args, stdout, stderr)
	case "validate":
		return validateConfig(args, stdout, stderr)
	case "healthcheck":
		return healthcheckEndpoint(args, stdout, stderr)
	case "version":
		return printVersion(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "epmon: unknown command %q (want run|validate|healthcheck|version)\n", args[0])
		return exitUsage
	}
}

// resolveConfigPath picks the config file: explicit --config flag first,
// then EPMON_CONFIG / ./epmon.yaml / /etc/epmon/epmon.yaml discovery,
// then the legacy config.yaml default.
func resolveConfigPath(args []string) string {
	if p := getConfigArg(args, ""); p != "" {
		return p
	}
	if p := config.DiscoverPath(""); p != "" {
		return p
	}
	return "config.yaml"
}

// runDefault runs epmon with the given configuration (default path, no subcommand).
func runDefault(args []string, stdout, stderr io.Writer) int {
	configPath := resolveConfigPath(args)
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "epmon: %v\n", err)
		return exitConfig
	}

	st, err := store.Open(context.Background(), cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		fmt.Fprintf(stderr, "epmon: %v\n", err)
		return exitStorage
	}
	defer st.Close()

	metas := make([]store.ServiceMeta, 0, len(cfg.Services))
	for _, svc := range cfg.Services {
		metas = append(metas, store.ServiceMeta{ID: svc.ID, Name: svc.Name, URL: svc.URL})
	}
	if err := st.SyncServices(context.Background(), metas, time.Now()); err != nil {
		fmt.Fprintf(stderr, "epmon: sync services: %v\n", err)
		return exitStorage
	}
	if n, err := st.Purge(context.Background(), cfg.Database.RetentionDays, time.Now()); err != nil {
		fmt.Fprintf(stderr, "epmon: initial purge: %v\n", err)
		return exitStorage
	} else if n > 0 {
		log.Printf("epmon: purged %d expired checks on boot", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(cfg.Server.APIKeys) == 0 {
		log.Printf("epmon: WARNING: no server.api_keys — write endpoints are unauthenticated")
	}

	registry := metrics.New()
	sched := scheduler.New(cfg, st, registry)
	sched.Run(ctx)
	defer sched.Stop()

	root := http.NewServeMux()
	root.Handle("/metrics", registry.Handler())
	root.Handle("/", api.New(cfg, st, nil).Handler())

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      api.Log(root),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	// serveErr carries a bind/serve failure back to runDefault so boot can
	// report exitListen instead of hanging until the next signal.
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("epmon: watching %d service(s), API on %s", len(cfg.Services), cfg.Server.Addr)
		var err error
		if cfg.Server.TLSCert != "" {
			err = srv.ListenAndServeTLS(cfg.Server.TLSCert, cfg.Server.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		fmt.Fprintf(stderr, "epmon: serve: %v\n", err)
		stop()
		return exitListen
	case <-ctx.Done():
	}

	log.Printf("epmon: shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return exitOK
}

// getConfigArg returns the value after -config/--config in args, or the
// default. Both dash forms (and --config=<path>) are accepted: the README,
// the usage strings and the Dockerfile all spell the single-dash form.
func getConfigArg(args []string, defaultPath string) string {
	for i, arg := range args {
		if (arg == "--config" || arg == "-config") && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(arg, "--config="); ok {
			return v
		}
	}
	return defaultPath
}

// validateConfig validates the given config file and exits 0 if valid, exitConfig otherwise.
// No database or network access is required.
// Usage: epmon validate --config <path>
func validateConfig(args []string, stdout, stderr io.Writer) int {
	configPath := resolveConfigPath(args[1:]) // skip "validate"
	_, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "epmon: validate error: %v\n", err)
		return exitConfig
	}
	fmt.Fprintf(stderr, "valid")
	return exitOK
}

// printVersion prints the epmon version, commit, and build date.
func printVersion(stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "epmon %s (commit %s, built %s, go %s)\n", version, commit, date, runtime.Version())
	return exitOK
}

// healthcheckTimeout bounds the readiness probe client. It matches the
// Dockerfile HEALTHCHECK --timeout so the CLI and the container agree.
var healthcheckTimeout = 5 * time.Second

// healthcheckEndpoint checks the health endpoint of the given URL and returns exitOK if healthy, exitUnavail otherwise.
// Usage: epmon healthcheck --endpoint <url>
func healthcheckEndpoint(args []string, stdout, stderr io.Writer) int {
	endpoint := getEndpointArg(args[1:]) // skip "healthcheck"
	if endpoint == "" {
		fmt.Fprintf(stderr, "epmon: healthcheck error: --endpoint required\n")
		return exitUsage
	}
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(strings.TrimSuffix(endpoint, "/") + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "epmon: healthcheck error: %v\n", err)
		return exitUnavail
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		fmt.Fprintf(stdout, "healthy")
		return exitOK
	}
	fmt.Fprintf(stderr, "epmon: healthcheck failed: status %d\n", resp.StatusCode)
	return exitUnavail
}

// getEndpointArg returns the value after -endpoint/--endpoint in args,
// or empty string. Both dash forms (and --endpoint=<url>) are accepted.
func getEndpointArg(args []string) string {
	for i, arg := range args {
		if (arg == "--endpoint" || arg == "-endpoint") && i+1 < len(args) {
			return args[i+1]
		}
		if v, ok := strings.CutPrefix(arg, "--endpoint="); ok {
			return v
		}
	}
	return ""
}

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	os.Exit(execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
