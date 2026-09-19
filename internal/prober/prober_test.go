package prober

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
)

func svc(url string) config.Service {
	spec := config.StatusSpec{}
	_ = spec.UnmarshalJSON([]byte("[200]"))
	return config.Service{
		ID:           "t",
		Name:         "t",
		URL:          url,
		Method:       "GET",
		Interval:     config.Duration(time.Minute),
		Timeout:      config.Duration(5 * time.Second),
		ExpectStatus: spec,
	}
}

func TestProbeUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	s := svc(srv.URL)
	s.BodyContains = "hello"
	check := Probe(t.Context(), s)
	if !check.Up || check.StatusCode != 200 || check.Error != "" {
		t.Errorf("expected up, got %+v", check)
	}
	if check.LatencyMs < 0 {
		t.Errorf("negative latency: %+v", check)
	}
}

func TestProbeDownCases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing-body" {
			w.Write([]byte("nope"))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	badStatus := svc(srv.URL)
	if c := Probe(t.Context(), badStatus); c.Up || c.StatusCode != 500 {
		t.Errorf("500 should be down: %+v", c)
	}

	badBody := svc(srv.URL + "/missing-body")
	badBody.BodyContains = "expected"
	if c := Probe(t.Context(), badBody); c.Up {
		t.Errorf("body mismatch should be down: %+v", c)
	}

	transport := svc("http://127.0.0.1:1/")
	transport.Timeout = config.Duration(2 * time.Second)
	if c := Probe(t.Context(), transport); c.Up || c.StatusCode != 0 || c.Error == "" {
		t.Errorf("refused conn should be down with error: %+v", c)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	timeoutSvc := svc(slow.URL)
	timeoutSvc.Timeout = config.Duration(50 * time.Millisecond)
	if c := Probe(t.Context(), timeoutSvc); c.Up {
		t.Errorf("timeout should be down: %+v", c)
	}
}

// TestProbeBodyCap constructs a body whose needle sits past a 100-byte
// cap: the capped read misses it (down) while the 4MiB backstop would
// have matched (up), proving the service-resolved probes.max_body_bytes
// governs inspection.
func TestProbeBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(append([]byte(strings.Repeat("x", 200)), "needle"...))
	}))
	defer srv.Close()

	s := svc(srv.URL)
	s.BodyContains = "needle"
	s.MaxBodyBytes = 100
	if c := Probe(t.Context(), s); c.Up {
		t.Errorf("capped probe should miss the needle: %+v", c)
	}

	uncapped := svc(srv.URL)
	uncapped.BodyContains = "needle"
	if c := Probe(t.Context(), uncapped); !c.Up {
		t.Errorf("backstop probe should match the needle: %+v", c)
	}
}

func TestProbeHeadersAndMethod(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.Header.Get("X-Test") != "yes" {
			w.WriteHeader(400)
			return
		}
	}))
	defer srv.Close()

	s := svc(srv.URL)
	s.Method = "HEAD"
	s.Headers = map[string]string{"X-Test": "yes"}
	if c := Probe(t.Context(), s); !c.Up {
		t.Errorf("expected up: %+v", c)
	}
}
