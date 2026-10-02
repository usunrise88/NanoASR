// Package docs serves the API reference at /docs.
//
// It is built from the dialects that are actually mounted rather than written
// out by hand, so a server documents itself: a deployment with the era dialect
// off does not advertise /asr, and a dialect added later appears here without
// anybody remembering to update a page. The dialects describe their own routes
// (adapter.Documented), which keeps the prose next to the code it is about.
//
// Served without a key, deliberately. Everything on this page is already
// knowable by anyone who can reach the port — what the endpoints are, what they
// take — and needing a credential to read the reference is how people end up
// guessing at an API instead of reading about it. No configuration, no model
// list and no job is on it; those stay behind authentication.
package docs

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

//go:embed page.html
var pageTemplate embed.FS

// Page is everything the reference says about this server.
type Page struct {
	Version string
	// AuthMode is "apikey" or "open".
	AuthMode string
	// Public lists the paths served without a credential.
	Public []string
	// UIPath is where the test UI is mounted, empty when it is not.
	UIPath string
	// Dialects are every dialect this build registers, the mounted ones first
	// in configuration order; adapter.DocsFor marks which are enabled.
	Dialects []adapter.Doc
	// Server holds the endpoints that belong to no dialect.
	Server []adapter.Route
	// Realtime describes the loaded streaming model, when there is one. It is
	// a function because the active session count changes between requests and
	// a snapshot taken at startup would be wrong by the time anyone read it.
	Realtime func() *core.RealtimeInfo
	// MaxUploadBytes is the body limit every dialect inherits.
	MaxUploadBytes int64
}

// Handler serves the page and the machine-readable description beside it.
func Handler(p Page) (http.Handler, error) {
	tmpl, err := template.New("page.html").Funcs(template.FuncMap{
		"mib": func(n int64) int64 { return n / (1 << 20) },
		"off": func(docs []adapter.Doc) []adapter.Doc {
			var out []adapter.Doc
			for _, d := range docs {
				if !d.Enabled {
					out = append(out, d)
				}
			}
			return out
		},
	}).ParseFS(pageTemplate, "page.html")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /docs/openapi.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(openAPI(p))
	})
	mux.HandleFunc("GET /docs/{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, tmpl, p)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		render(w, r, tmpl, p)
	})
	return mux, nil
}

type view struct {
	Page
	RealtimeInfo *core.RealtimeInfo
}

func render(w http.ResponseWriter, _ *http.Request, tmpl *template.Template, p Page) {
	v := view{Page: p}
	if p.Realtime != nil {
		v.RealtimeInfo = p.Realtime()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A failed render would otherwise be a half-written page with a 200 on it.
	var buf strings.Builder
	if err := tmpl.Execute(&buf, v); err != nil {
		http.Error(w, "the documentation page could not be rendered", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(buf.String()))
}

// --- OpenAPI ----------------------------------------------------------------

// openAPI renders the route table as an OpenAPI document.
//
// Abridged on purpose, and it says so in its own description: the request and
// response schemas are not in it. Writing them out by hand would create a
// second description of every endpoint that drifts from the first, and the one
// thing worse than no schema is a schema that is wrong. What this is for is
// importing the endpoint list into a client generator or an API browser.
func openAPI(p Page) map[string]any {
	paths := map[string]any{}

	add := func(r adapter.Route, tag string) {
		// A query string is not part of an OpenAPI path.
		path, query, hasQuery := strings.Cut(r.Path, "?")
		detail := r.Detail
		if hasQuery {
			if detail != "" {
				detail += " "
			}
			detail += "Called with the query string " + query + "."
		}
		switch {
		case r.Admin:
			detail = strings.TrimSpace(detail + " Requires an administrative API key.")
		case r.Public:
			detail = strings.TrimSpace(detail + " Served without a credential.")
		}

		item, _ := paths[path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[path] = item
		}
		item[strings.ToLower(r.Method)] = map[string]any{
			"summary":     r.Summary,
			"description": detail,
			"tags":        []string{tag},
			"responses": map[string]any{
				"default": map[string]any{"description": "See the description."},
			},
		}
	}

	for _, d := range p.Dialects {
		// A dialect that is off serves none of these, and a generated client
		// that called them would get a 404. The page names it in prose; this
		// file lists only what answers.
		if !d.Enabled {
			continue
		}
		for _, r := range d.Routes {
			add(r, d.Name)
		}
	}
	for _, r := range p.Server {
		add(r, "server")
	}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "NanoASR",
			"version": p.Version,
			"description": "Offline speech recognition. This description is generated from the " +
				"dialects this server has mounted and lists every endpoint it serves; " +
				"request and response schemas are deliberately absent rather than " +
				"hand-written and wrong. The prose at /docs is the reference.",
		},
		"paths": paths,
	}
}
