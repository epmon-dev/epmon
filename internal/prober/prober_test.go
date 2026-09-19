package prober

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestProbeFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusMovedPermanently)
			return
		}
		_, _ = w.Write([]byte("target ok"))
	}))
	defer srv.Close()

	off := false
	withoutFollow := svc(srv.URL + "/redirect")
	withoutFollow.FollowRedirects = &off
	if c := Probe(t.Context(), withoutFollow); c.Up || c.StatusCode != 301 {
		t.Errorf("follow_redirects=false should record 301/down, got %+v", c)
	}

	// With the flag on (or unset), today's behavior is unchanged.
	if c := Probe(t.Context(), svc(srv.URL+"/redirect")); !c.Up || c.StatusCode != 200 {
		t.Errorf("follow_redirects default should land on 200/up, got %+v", c)
	}

	// A redirect status can itself be expected.
	expect301 := svc(srv.URL + "/redirect")
	expect301.FollowRedirects = &off
	spec := config.StatusSpec{}
	if err := spec.UnmarshalJSON([]byte("[301]")); err != nil {
		t.Fatal(err)
	}
	expect301.ExpectStatus = spec
	if c := Probe(t.Context(), expect301); !c.Up || c.StatusCode != 301 {
		t.Errorf("expected 301 should be up, got %+v", c)
	}
}

func TestProbeReusesConnections(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		conns.Add(1)
		return ctx
	}
	srv.Start()
	defer srv.Close()

	s := svc(srv.URL)
	for i := 0; i < 2; i++ {
		if c := Probe(t.Context(), s); !c.Up || c.StatusCode != 200 {
			t.Fatalf("probe %d: expected up, got %+v", i, c)
		}
	}
	if got := conns.Load(); got != 1 {
		t.Errorf("two sequential probes used %d connections, want 1 (keep-alive reuse)", got)
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
