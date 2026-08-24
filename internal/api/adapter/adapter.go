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
	// MaxUploadBytes is enforced by middleware too; dialects need it to report
	// the limit in their own error shape.
	MaxUploadBytes int64
	// ConfigSnapshot returns the effective configuration with secrets already
	// redacted. It is a function rather than a value so a dialect cannot be
	// handed the live struct and reach into it: what comes back is whatever the
	// server decided is safe to show.
	ConfigSnapshot func() any
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
