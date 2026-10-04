// Package webui embeds the single-page operator console.
//
// The page is deliberately dependency-free: one index.html compiled into the
// binary with go:embed, native JS only, no build step and no external CDN, so
// the console keeps working on offline LANs and NAS deployments.
package webui

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler serves the embedded console at "/" (and "/index.html").
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/index.html":
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The console must never be cached: an upgraded container ships a
		// newer page behind the same URL.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexHTML)
	})
}
