//go:build !noui

// Package ui serves the built SPA from inside the binary. There is no Node
// server in production: `make web` writes the Vite output into dist/ and it is
// embedded at compile time.
//
// Building with -tags noui removes the SPA entirely, for headless deployments.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var assets embed.FS

// Enabled reports whether this build contains the UI.
const Enabled = true

// Handler serves the SPA under prefix. Unknown paths fall back to index.html so
// client-side routing works on a hard refresh; asset paths do not, so a missing
// asset is still a 404 rather than an HTML page with a JavaScript content type.
func Handler(prefix string) (http.Handler, error) {
	sub, err := fs.Sub(assets, "dist")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))

	return http.StripPrefix(strings.TrimSuffix(prefix, "/"),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := strings.TrimPrefix(r.URL.Path, "/")
			if p == "" {
				p = "index.html"
			}
			if _, err := fs.Stat(sub, p); err != nil {
				if strings.Contains(p, ".") {
					http.NotFound(w, r)
					return
				}
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; "+
					"style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'")
			files.ServeHTTP(w, r)
		})), nil
}
