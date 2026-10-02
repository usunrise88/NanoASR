// Package adapter is how NanoASR speaks more than one API dialect.
//
// A dialect is one Go package that registers itself from init() and mounts its
// routes onto the shared mux. Core logic lives behind core.Service and never
// learns which dialect a request arrived through, so adding a dialect is adding
// a file (SPEC §8.3), not threading a new shape through the pipeline.
package adapter

import (
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Deps is what a dialect is allowed to reach. Note the absence of the model
// pool, the registry and the queue: a dialect that needs them is a sign the
// capability belongs in core.Service instead.
type Deps struct {
	Models core.ModelService
	// Realtime is nil unless a streaming model is loaded, which is the normal
	// case: a dialect that needs it must say so rather than assume.
	Realtime core.RealtimeService
	// MaxUploadBytes is enforced by middleware too; dialects need it to report
	// the limit in their own error shape.
	MaxUploadBytes int64
	// ConfigSnapshot returns the effective configuration with secrets already
	// redacted. It is a function rather than a value so a dialect cannot be
	// handed the live struct and reach into it: what comes back is whatever the
	// server decided is safe to show.
	ConfigSnapshot func() any
}

// Route is one endpoint, as the documentation page describes it.
type Route struct {
	Method  string
	Path    string
	Summary string
	// Detail is a sentence or two: what the endpoint takes, and anything about
	// it that would otherwise be a surprise.
	Detail string
	// Admin marks an endpoint that needs an administrative key.
	Admin bool
	// Public marks one that needs no key at all.
	Public bool
}

// Doc describes a dialect for /docs.
type Doc struct {
	Name    string
	Title   string
	Summary string
	Routes  []Route
}

// Documented is the optional half of Adapter: a dialect that can describe
// itself appears in the documentation page.
//
// Optional rather than required because the page is built from the dialects
// actually mounted, and a dialect nobody enabled must not be documented as
// though it were serving. A dialect that does not implement this is listed by
// name with a note saying it describes no routes, which is a visible gap
// rather than a silent one.
type Documented interface {
	Doc() Doc
}

// DocsFor returns the documentation of the named dialects, in the order given.
func DocsFor(names []string) []Doc {
	mu.RLock()
	defer mu.RUnlock()

	out := make([]Doc, 0, len(names))
	for _, n := range names {
		a, ok := adapters[n]
		if !ok {
			continue
		}
		d, ok := a.(Documented)
		if !ok {
			out = append(out, Doc{
				Name:    n,
				Title:   n,
				Summary: "This dialect does not describe its routes.",
			})
			continue
		}
		doc := d.Doc()
		if doc.Name == "" {
			doc.Name = n
		}
		out = append(out, doc)
	}
	return out
}

// Adapter is one API dialect.
type Adapter interface {
	// Name is the identifier used in config's api.dialects.
	Name() string
	// Mount registers routes. It must not start goroutines or touch global
	// state; everything it needs arrives through svc and deps.
	Mount(mux *http.ServeMux, svc core.Service, deps Deps)
}

var (
	mu       sync.RWMutex
	adapters = map[string]Adapter{}
)

// Register is called from a dialect package's init().
func Register(a Adapter) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := adapters[a.Name()]; dup {
		panic(fmt.Sprintf("adapter: dialect %q registered twice", a.Name()))
	}
	adapters[a.Name()] = a
}

// Available lists registered dialect names, sorted.
func Available() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(adapters))
	for name := range adapters {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// MountAll mounts the named dialects, failing on an unknown name rather than
// silently serving less than the operator asked for.
func MountAll(mux *http.ServeMux, names []string, svc core.Service, deps Deps) error {
	mu.RLock()
	defer mu.RUnlock()
	for _, n := range names {
		a, ok := adapters[n]
		if !ok {
			return fmt.Errorf("unknown api dialect %q (available: %v)", n, Available())
		}
		a.Mount(mux, svc, deps)
	}
	return nil
}
