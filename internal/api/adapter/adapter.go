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
	// Enabled is false for a dialect this build registers but the
	// configuration does not list. Its routes are not served, and the page
	// says so rather than leaving them out: somebody looking for an endpoint
	// the release notes promised needs to find out that it is one line of
	// configuration away, not that it does not exist.
	Enabled bool
}

// Documented is the optional half of Adapter: a dialect that can describe
// itself appears in the documentation page.
//
// Optional rather than required because a dialect nobody enabled must not be
// documented as though it were serving. A dialect that does not implement this
// is listed by name with a note saying it describes no routes, which is a
// visible gap rather than a silent one — and so is a dialect that is off: it
// is listed as off, with its routes withheld.
type Documented interface {
	Doc() Doc
}

// DocsFor returns the documentation of every dialect this build registers: the
// enabled ones first, in the order the configuration named them, then the rest
// marked Enabled=false and sorted by name.
//
// Returning the disabled ones too is the point. Enabling a dialect is one line
// in api.dialects, and a page that simply omitted what is off left somebody
// who had just read about an endpoint with no way to tell whether it was
// missing from the build, missing from the release, or merely switched off.
func DocsFor(names []string) []Doc {
	mu.RLock()
	defer mu.RUnlock()

	out := make([]Doc, 0, len(adapters))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		a, ok := adapters[n]
		if !ok {
			continue
		}
		seen[n] = true
		out = append(out, docOf(n, a, true))
	}

	rest := make([]string, 0, len(adapters))
	for n := range adapters {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	for _, n := range rest {
		out = append(out, docOf(n, adapters[n], false))
	}
	return out
}

func docOf(name string, a Adapter, enabled bool) Doc {
	d, ok := a.(Documented)
	if !ok {
		return Doc{
			Name:    name,
			Title:   name,
			Summary: "This dialect does not describe its routes.",
			Enabled: enabled,
		}
	}
	doc := d.Doc()
	if doc.Name == "" {
		doc.Name = name
	}
	if doc.Title == "" {
		doc.Title = name
	}
	doc.Enabled = enabled
	return doc
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
