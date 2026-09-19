package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
)

func TestNewHTTPServerUsesConfigTimeouts(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Addr = "127.0.0.1:18080"
	cfg.Server.ReadTimeout = config.Duration(7 * time.Second)
	cfg.Server.WriteTimeout = config.Duration(11 * time.Second)

	srv := newHTTPServer(cfg, http.NewServeMux())
	if srv.Addr != "127.0.0.1:18080" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.ReadTimeout != 7*time.Second {
		t.Errorf("ReadTimeout = %v, want 7s (must follow server.read_timeout)", srv.ReadTimeout)
	}
	if srv.WriteTimeout != 11*time.Second {
		t.Errorf("WriteTimeout = %v, want 11s (must follow server.write_timeout)", srv.WriteTimeout)
	}
}

func TestSqliteDSNCarriesBusyTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = "epmon.db"
	cfg.Storage.BusyTimeoutMs = 2500

	got := sqliteDSN(cfg)
	if !strings.Contains(got, "busy_timeout(2500)") {
		t.Errorf("sqlite DSN = %q, want busy_timeout(2500) applied", got)
	}

	cfg.Database.Driver = "postgres"
	cfg.Database.DSN = "postgres://db/x"
	if got := sqliteDSN(cfg); got != "postgres://db/x" {
		t.Errorf("non-sqlite DSN = %q, want untouched", got)
	}
}
