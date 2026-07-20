package main

// The audit UI (ui/ — React + Tailwind, built by `make ui`) is embedded so
// deployment stays one binary: localhost-bound, offline-capable, zero
// runtime CDN. The UI is a pure driving adapter over the same JSON API the
// CLI uses — nothing visible here that `krakoactl why` can't derive.

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:uidist
var uiFS embed.FS

func uiHandler() http.Handler {
	dist, err := fs.Sub(uiFS, "uidist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/ui")
		p = strings.TrimPrefix(p, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			p = "index.html" // SPA fallback: unknown paths render the app
		}
		if p == "index.html" {
			// serve directly — http.FileServer 301-redirects /index.html
			data, err := fs.ReadFile(dist, "index.html")
			if err != nil {
				http.Error(w, "ui not built", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + p
		fileServer.ServeHTTP(w, r2)
	})
}
