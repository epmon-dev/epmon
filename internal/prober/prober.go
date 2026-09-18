// Package prober executes a single HTTP check against a configured service.
package prober

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/epmon-dev/epmon/internal/config"
	"github.com/epmon-dev/epmon/internal/store"
)

// maxBody is the cap for body_contains inspection (4 MiB).
const maxBody = 4 << 20

// Probe performs one check. Transport errors, unexpected status codes and
// body mismatches all report Up=false with a short machine-readable error.
// ctx cancels the request; the timeout still bounds the whole attempt.
func Probe(ctx context.Context, svc config.Service) store.Check {
	start := time.Now()
	check := store.Check{ServiceID: svc.ID, TS: start.Unix()}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if svc.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	client := &http.Client{Transport: transport, Timeout: svc.Timeout.Std()}

	ctx, cancel := context.WithTimeout(ctx, svc.Timeout.Std())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, svc.Method, svc.URL, nil)
	if err != nil {
		check.Error = "bad-request: " + err.Error()
		return check
	}
	for k, v := range svc.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "epmon/1.0")
	}

	res, err := client.Do(req)
	check.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		check.Error = "transport: " + shortErr(err)
		return check
	}
	defer res.Body.Close()
	check.StatusCode = res.StatusCode

	ok := svc.ExpectStatus.Matches(res.StatusCode)
	if !ok {
		check.Error = fmt.Sprintf("status: got %d", res.StatusCode)
		return check
	}
	if svc.BodyContains != "" {
		body, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
		if err != nil {
			check.Error = "body: " + shortErr(err)
			return check
		}
		if !strings.Contains(string(body), svc.BodyContains) {
			check.Error = "body: expected substring not found"
			return check
		}
	}
	check.Up = true
	return check
}

func shortErr(err error) string {
	msg := err.Error()
	if len(msg) > 160 {
		return msg[:160]
	}
	return msg
}
