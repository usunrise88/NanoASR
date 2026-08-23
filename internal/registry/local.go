package registry

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/usunrise88/nanoasr/internal/core"
)

// ManifestFile is the per-model descriptor placed beside the weights.
const ManifestFile = "model.yaml"

// Local resolves models already present on disk. It never reaches the network:
// downloading is a separate concern layered on top, so an air-gapped
// deployment runs this and nothing else.
type Local struct {
	root string

	mu       sync.RWMutex
	byKey    map[string]entry // "id" and "id@revision" both index the same entry
	scanErrs []string
}

type entry struct {
	manifest Manifest
	dir      string
}

// NewLocal scans root once. A missing directory is not an error: a fresh
// install has no models yet, and the failure should surface when a request
// names a model, not at boot.
func NewLocal(root string) (*Local, error) {
	l := &Local{root: root, byKey: map[string]entry{}}
	if err := l.Refresh(context.Background()); err != nil {
		return nil, err
	}
	return l, nil
}

// Refresh rescans the models directory, picking up models added since start.
func (l *Local) Refresh(ctx context.Context) error {
	found := map[string]entry{}
	var problems []string

	dirents, err := os.ReadDir(l.root)
	if err != nil {
		if !os.IsNotExist(err) {
			return core.Errorf(core.CodeInternal,
				"cannot read models directory %s", l.root).WithCause(err)
		}
		dirents = nil
	}

	for _, de := range dirents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(l.root, de.Name())

		m, err := ReadManifest(filepath.Join(dir, ManifestFile))
		if err != nil {
			if os.IsNotExist(err) {
				continue // a directory without a manifest is simply not a model
			}
			// One broken manifest must not hide every other model; record it
			// and keep going.
			problems = append(problems, de.Name()+": "+err.Error())
			continue
		}

		e := entry{manifest: m, dir: dir}
		found[m.Key()] = e
		// The bare id points at the highest revision present, so a request
		// without a revision is deterministic rather than map-order luck.
		if prev, ok := found[m.ID]; !ok || m.Revision > prev.manifest.Revision {
			found[m.ID] = e
		}
	}

	l.mu.Lock()
	l.byKey, l.scanErrs = found, problems
	l.mu.Unlock()
	return nil
}

// Problems lists manifests that failed to load, for startup logging. A model
// nobody can use should be visible, not silently absent.
func (l *Local) Problems() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]string(nil), l.scanErrs...)
}

func (l *Local) Resolve(_ context.Context, id string) (Manifest, error) {
	e, err := l.lookup(id)
	if err != nil {
		return Manifest{}, err
	}
	return e.manifest, nil
}

func (l *Local) Local(_ context.Context) ([]Manifest, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	seen := map[string]bool{}
	out := make([]Manifest, 0, len(l.byKey))
	for _, e := range l.byKey {
		if seen[e.dir] {
			continue // the same model is indexed under two keys
		}
		seen[e.dir] = true
		out = append(out, e.manifest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// Catalog lists what could be downloaded. It is the built-in catalog verbatim:
// fetching is not this type's job.
func (l *Local) Catalog(_ context.Context) ([]Manifest, error) {
	return Builtin()
}

// Ensure returns the model's directory. It never downloads; a missing model is
// an error naming what is available.
func (l *Local) Ensure(_ context.Context, id string) (string, error) {
	e, err := l.lookup(id)
	if err != nil {
		return "", err
	}
	return e.dir, nil
}

// Fetch cannot download, so it reports what is already present and nothing
// else. Remote is the type that fetches.
func (l *Local) Fetch(_ context.Context, id string) (<-chan core.DownloadProgress, error) {
	if _, err := l.lookup(id); err != nil {
		return nil, err
	}
	ch := make(chan core.DownloadProgress, 1)
	ch <- core.DownloadProgress{ModelID: id, Percent: 100, Done: true}
	close(ch)
	return ch, nil
}

func (l *Local) Dir(id string) (string, error) {
	e, err := l.lookup(id)
	if err != nil {
		return "", err
	}
	return e.dir, nil
}

func (l *Local) lookup(id string) (entry, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if e, ok := l.byKey[id]; ok {
		return e, nil
	}
	return entry{}, core.Errorf(core.CodeModelNotFound,
		"model %q is not present in %s (available: %s)", id, l.root, strings.Join(l.namesLocked(), ", "))
}

func (l *Local) namesLocked() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range l.byKey {
		if seen[e.dir] {
			continue
		}
		seen[e.dir] = true
		out = append(out, e.manifest.Key())
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// ReadManifest loads and validates one model.yaml.
func ReadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}

	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	// An unknown key is almost always a typo in a field that matters, such as
	// feature dim; failing is better than silently using a default.
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, core.Errorf(core.CodeInvalidRequest,
			"cannot parse %s", path).WithCause(err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

var _ Registry = (*Local)(nil)
