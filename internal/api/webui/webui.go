// Package webui serves the vendored status-page app (spec §9) from the
// epmon binary: the built output of epmon-dev/status, embedded at compile
// time. Runtime stays Node-free — bundling happens once, in the status
// repo; see make web / scripts/refresh-webui.sh for the refresh flow.
//
// Routing contract: exact files win (index.html, /assets/*, /embed.js,
// /favicon.svg); every other path falls back to index.html so the SPA's
// /p/:slug, /s/:domain and /embed routes render. HTML is always
// no-store; hashed /assets/* are immutable; /embed.js is versioned by
// content hash in practice but served with a 1h cap per the app's
// hosting contract.
package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed index.html favicon.svg embed.js assets
var embedded embed.FS

// files is the embedded tree rooted at the vendored dir.
var files, _ = fs.Sub(embedded, ".")

// Handler serves the app. Only GET/HEAD; anything else is 405.
func Handler() http.Handler {
	fsrv := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		if p != "/" && exists(p) {
			cachePolicy(w, p)
			fsrv.ServeHTTP(w, r)
			return
		}
		// SPA fallback (and "/"): always the shell, never cached.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "status UI unavailable", http.StatusInternalServerError)
			return
		}
		now := time.Now()
		w.Header().Set("Last-Modified", now.UTC().Format(http.TimeFormat))
		http.ServeContent(w, r, "index.html", now, bytes.NewReader(data))
	})
}

func exists(p string) bool {
	f, err := files.Open(strings.TrimPrefix(p, "/"))
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	return err == nil && !st.IsDir()
}

// cachePolicy assigns Cache-Control per the app's hosting contract:
// hashed assets immutable, badge script bounded, everything else none
// (HTML shell goes through the no-store fallback above).
func cachePolicy(w http.ResponseWriter, p string) {
	switch {
	case strings.HasPrefix(p, "/assets/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case p == "/embed.js":
		w.Header().Set("Cache-Control", "public, max-age=3600")
	case p == "/favicon.svg":
		w.Header().Set("Cache-Control", "public, max-age=86400")
	default:
		w.Header().Set("Cache-Control", "no-store")
	}
}
