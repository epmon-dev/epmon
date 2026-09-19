package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetConfigArg(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"double dash":   {[]string{"run", "--config", "a.yaml"}, "a.yaml"},
		"single dash":   {[]string{"-config", "a.yaml"}, "a.yaml"},
		"equals form":   {[]string{"--config=a.yaml"}, "a.yaml"},
		"validate":      {[]string{"--config", "v.yaml"}, "v.yaml"},
		"missing":       {[]string{"run"}, "config.yaml"},
		"flag no value": {[]string{"--config"}, "config.yaml"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := getConfigArg(tc.args, "config.yaml"); got != tc.want {
				t.Errorf("getConfigArg(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestGetEndpointArg(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"double dash": {[]string{"--endpoint", "http://x/"}, "http://x/"},
		"single dash": {[]string{"-endpoint", "http://x/"}, "http://x/"},
		"equals form": {[]string{"--endpoint=http://x/"}, "http://x/"},
		"missing":     {[]string{}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := getEndpointArg(tc.args); got != tc.want {
				t.Errorf("getEndpointArg(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestHealthcheckEndpoint(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	if code := healthcheckEndpoint([]string{"healthcheck", "--endpoint", healthy.URL}, io.Discard, io.Discard); code != exitOK {
		t.Errorf("healthy = %d, want %d", code, exitOK)
	}
	// Trailing slash must not produce //healthz.
	if code := healthcheckEndpoint([]string{"healthcheck", "-endpoint", healthy.URL + "/"}, io.Discard, io.Discard); code != exitOK {
		t.Errorf("trailing slash = %d, want %d", code, exitOK)
	}
	if code := healthcheckEndpoint([]string{"healthcheck"}, io.Discard, io.Discard); code != exitUsage {
		t.Errorf("missing endpoint = %d, want %d", code, exitUsage)
	}
	if code := healthcheckEndpoint([]string{"healthcheck", "--endpoint", "http://127.0.0.1:1/"}, io.Discard, io.Discard); code != exitUnavail {
		t.Errorf("refused = %d, want %d", code, exitUnavail)
	}
}

// TestHealthcheckTimeout asserts the subcommand gives up on a hanging
// server instead of blocking forever.
func TestHealthcheckTimeout(t *testing.T) {
	old := healthcheckTimeout
	healthcheckTimeout = 200 * time.Millisecond
	defer func() { healthcheckTimeout = old }()

	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hanging.Close()

	start := time.Now()
	code := healthcheckEndpoint([]string{"healthcheck", "--endpoint", hanging.URL}, io.Discard, io.Discard)
	if elapsed := time.Since(start); code != exitUnavail || elapsed > 10*time.Second {
		t.Errorf("hanging = %d in %v, want %d quickly", code, elapsed, exitUnavail)
	}
}
