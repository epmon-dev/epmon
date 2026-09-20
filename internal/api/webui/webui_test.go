package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// firstAsset finds a vendored bundle at runtime: hashed filenames change
// on every status/ rebuild, so no hash may appear literally here.
func firstAsset(t *testing.T, suffix string) string {
	t.Helper()
	entries, err := fs.ReadDir(files, "assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			return "/assets/" + e.Name()
		}
	}
	t.Fatalf("no %q asset vendored", suffix)
	return ""
}

func get(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Header(), rec.Body.String()
}

func TestRootRedirectsToLocal(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/local" {
		t.Errorf("GET / Location = %q, want /local", loc)
	}
}

func TestIndexAndFallbackAreNoStoreHTML(t *testing.T) {
	for _, path := range []string{"/local", "/p/acme", "/s/status.example.com", "/nope", "/embed"} {
		code, header, body := get(t, path)
		if code != 200 {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
		if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q, want text/html", path, ct)
		}
		if cc := header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", path, cc)
		}
		if !strings.Contains(body, `<div id="root">`) {
			t.Errorf("GET %s is not the app shell", path)
		}
	}
}

func TestAssetCachePolicy(t *testing.T) {
	code, header, body := get(t, firstAsset(t, ".js"))
	if code != 200 || len(body) == 0 {
		t.Fatalf("asset = %d bytes, code %d", len(body), code)
	}
	if cc := header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q", cc)
	}
	if _, header, _ := get(t, "/embed.js"); header.Get("Cache-Control") != "public, max-age=3600" {
		t.Errorf("embed.js Cache-Control = %q", header.Get("Cache-Control"))
	}
	if code, _, _ := get(t, "/favicon.svg"); code != 200 {
		t.Errorf("favicon = %d, want 200", code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d, want 405", rec.Code)
	}
}
