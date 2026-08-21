package sherpa

import (
	"fmt"
	"sort"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// Family adapts one sherpa-onnx model family to a NanoASR manifest.
//
// Supporting a new family is adding one file to this package: declare which
// files it needs, what it can produce, and where its paths go in the
// sherpa-onnx config. Nothing outside this package changes.
//
// The family adapters live here rather than in a neutral package because
// Configure has to touch sonnx types, and cgo is deliberately confined to this
// subtree — the alternative was writing the same mapping twice.
type Family interface {
	// Name is the manifest's family field.
	Name() string

	// Validate checks that the manifest names every file this family needs,
	// before anything is downloaded or loaded.
	Validate(files map[string]string) error

	// Capabilities is what this family can produce at best. The manifest may
	// narrow it; it may not widen it.
	Capabilities() core.Capabilities

	// Configure points the sherpa-onnx model config at this model's files.
	// Paths in the manifest are relative to dir.
	Configure(m registry.Manifest, dir string, cfg *sonnx.OfflineModelConfig) error
}

var (
	familiesMu sync.RWMutex
	families   = map[string]Family{}
)

// RegisterFamily is called from a family file's init().
func RegisterFamily(f Family) {
	familiesMu.Lock()
	defer familiesMu.Unlock()
	if _, dup := families[f.Name()]; dup {
		panic(fmt.Sprintf("sherpa: family %q registered twice", f.Name()))
	}
	families[f.Name()] = f
}

// LookupFamily returns the adapter for a manifest family.
func LookupFamily(name string) (Family, error) {
	familiesMu.RLock()
	defer familiesMu.RUnlock()
	f, ok := families[name]
	if !ok {
		return nil, core.Errorf(core.CodeModelNotFound,
			"unknown model family %q (known: %v)", name, familyNamesLocked())
	}
	return f, nil
}

// Families lists registered family names, sorted, for diagnostics and docs.
func Families() []string {
	familiesMu.RLock()
	defer familiesMu.RUnlock()
	return familyNamesLocked()
}

func familyNamesLocked() []string {
	out := make([]string, 0, len(families))
	for name := range families {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// requireFiles reports the first required role the manifest does not name.
func requireFiles(files map[string]string, roles ...string) error {
	for _, r := range roles {
		if files[r] == "" {
			return core.Errorf(core.CodeInvalidRequest,
				"manifest is missing required file %q", r)
		}
	}
	return nil
}
