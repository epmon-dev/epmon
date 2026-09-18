// Production middleware: panic recovery, security headers, CORS,
// per-IP rate limiting, bearer auth for writes, request body caps.
// Each is a small decorator so the chain in Handler reads top-down.
package api

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// recoverer turns handler panics into JSON 500s instead of dropped connections.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeErr(w, http.StatusInternalServerError, "internal", "unexpected error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets baseline hardening headers. HSTS is intentionally
// absent — TLS terminates at the operator's reverse proxy (see README).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// cors gates cross-origin browser access. Empty origins = same-origin only
// (no headers emitted). "*" allows any origin; otherwise exact match.
func cors(allowed []string, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	star := false
	set := map[string]bool{}
	for _, o := range allowed {
		if o == "*" {
			star = true
		} else {
			set[o] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allow := ""
		switch {
		case star:
			allow = "*"
		case set[origin]:
			allow = origin
		}
		if allow == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", allow)
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP resolves the rate-limit/auth bucket. X-Forwarded-For is honored
// only with trustProxy — behind a sanitizing proxy it is the real client,
// on the open internet it is attacker-controlled.
func clientIP(trustProxy bool, r *http.Request) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.Index(xff, ","); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

type bucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

// rateLimiter is a per-IP token bucket. rpm<=0 disables limiting.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rpm     int
	burst   int
	now     func() time.Time
}

func newRateLimiter(rpm, burst int) *rateLimiter {
	return &rateLimiter{
		buckets: map[string]*bucket{},
		rpm:     rpm,
		burst:   burst,
		now:     time.Now,
	}
}

// allow reports whether the request may proceed, and how long to wait otherwise.
func (l *rateLimiter) allow(ip string) (bool, time.Duration) {
	if l.rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(l.rpm)), l.burst)}
		l.buckets[ip] = b
	}
	b.seen = l.now()
	// Opportunistic sweep so the map can't grow with spoofed IPs forever.
	if len(l.buckets)%512 == 0 {
		cutoff := l.now().Add(-10 * time.Minute)
		for ip, b := range l.buckets {
			if b.seen.Before(cutoff) {
				delete(l.buckets, ip)
			}
		}
	}
	reservation := b.limiter.Reserve()
	delay := reservation.Delay()
	if delay > 0 {
		reservation.Cancel()
		return false, delay
	}
	return true, 0
}

func (s *Server) limitByIP(l *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := l.allow(clientIP(s.cfg.Server.TrustProxy, r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "slow down and retry")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireWriteAuth gates mutating methods behind bearer keys. Safe methods
// stay public (status pages are meant to be read). No keys configured means
// auth is off — main logs a loud warning in that case.
func (s *Server) requireWriteAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.Server.APIKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, key := range s.cfg.Server.APIKeys {
			if subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "unauthorized", "valid bearer key required")
	})
}

// limitBody caps JSON payloads before handlers decode them.
func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodPut:
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// isTooLarge reports whether err came from the body cap.
func isTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
