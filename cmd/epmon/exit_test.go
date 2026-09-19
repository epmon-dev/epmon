package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveConfigPath(t *testing.T) {
	// Explicit flag always wins, even when discovery would find something.
	t.Setenv("EPMON_CONFIG", filepath.Join(t.TempDir(), "env.yaml"))
	if got := resolveConfigPath([]string{"run", "--config", "flag.yaml"}); got != "flag.yaml" {
		t.Errorf("explicit flag = %q, want flag.yaml", got)
	}
	// No flag: EPMON_CONFIG is honored.
	if got := resolveConfigPath([]string{"run"}); got != os.Getenv("EPMON_CONFIG") {
		t.Errorf("discovery = %q, want $EPMON_CONFIG", got)
	}

	// Nothing anywhere: legacy default (guarded against stray files).
	t.Setenv("EPMON_CONFIG", "")
	if _, err := os.Stat("/etc/epmon/epmon.yaml"); err == nil {
		t.Skip("machine has /etc/epmon/epmon.yaml; discovery legitimately prefers it")
	}
	if _, err := os.Stat("epmon.yaml"); err == nil {
		t.Skip("package dir has epmon.yaml; discovery legitimately prefers it")
	}
	if got := resolveConfigPath([]string{"run"}); got != "config.yaml" {
		t.Errorf("fallback = %q, want config.yaml", got)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "epmon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteExitCodes(t *testing.T) {
	ctx := context.Background()

	if code := execute(ctx, []string{"bogus"}, io.Discard, io.Discard); code != exitUsage {
		t.Errorf("unknown command = %d, want %d", code, exitUsage)
	}
	if code := execute(ctx, []string{"validate", "--config", filepath.Join(t.TempDir(), "nope.yaml")}, io.Discard, io.Discard); code != exitConfig {
		t.Errorf("validate missing = %d, want %d", code, exitConfig)
	}

	badDriver := writeConfig(t, `
server:
  addr: "127.0.0.1:18081"
database:
  driver: nope
  dsn: x
services:
  - {id: a, url: http://127.0.0.1:1/}
`)
	if code := execute(ctx, []string{"run", "--config", badDriver}, io.Discard, io.Discard); code != exitStorage {
		t.Errorf("run bad driver = %d, want %d", code, exitStorage)
	}
}

func TestExecuteExitListen(t *testing.T) {
	// Occupy a port so the server bind fails fast.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer ln.Close()

	cfg := writeConfig(t, fmt.Sprintf(`
server:
  addr: %q
database:
  driver: sqlite
  dsn: %q
services:
  - {id: a, url: http://127.0.0.1:1/}
`, ln.Addr().String(), filepath.Join(t.TempDir(), "test.db")))

	done := make(chan int, 1)
	go func() {
		done <- execute(context.Background(), []string{"run", "--config", cfg}, io.Discard, io.Discard)
	}()
	select {
	case code := <-done:
		if code != exitListen {
			t.Errorf("run bind conflict = %d, want %d", code, exitListen)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after bind failure")
	}
}
