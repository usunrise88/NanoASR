// Package service implements the administrative surface that sits above the
// registry and the model pool.
package service

import (
	"context"
	"sort"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// Models implements core.ModelService over the registry and the pool.
//
// Load, unload, pin and hot swap all exist already because the pool implements
// them; only downloading is deferred, and it says so rather than pretending.
type Models struct {
	registry registry.Registry
	pool     *pool.Pool
}

func NewModels(reg registry.Registry, p *pool.Pool) *Models {
	return &Models{registry: reg, pool: p}
}

// List merges what is on disk with what is resident, so one call answers both
// "what can I ask for" and "what is warm right now".
func (m *Models) List(ctx context.Context) ([]core.ModelInfo, error) {
	local, err := m.registry.Local(ctx)
	if err != nil {
		return nil, err
	}

	resident := map[string]core.ModelInfo{}
	for _, info := range m.pool.List() {
		resident[info.ID] = info
	}

	out := make([]core.ModelInfo, 0, len(local))
	for _, man := range local {
		if info, ok := resident[man.ID]; ok {
			out = append(out, info)
			delete(resident, man.ID)
			continue
		}
		out = append(out, describe(man, core.ModelAbsent))
	}

	// A model can be resident without being on disk any more — someone deleted
	// the directory while it was loaded. Reporting it is more useful than
	// hiding it.
	for _, info := range resident {
		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Models) Catalog(ctx context.Context) ([]core.ModelInfo, error) {
	entries, err := m.registry.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.ModelInfo, 0, len(entries))
	for _, man := range entries {
		out = append(out, describe(man, core.ModelAbsent))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Download is the one operation that needs the network, which is a separate
// milestone. Until then a model has to be placed in the models directory.
func (m *Models) Download(context.Context, string) (<-chan core.DownloadProgress, error) {
	return nil, core.Errorf(core.CodeNotImplemented,
		"downloading is not available in this build; place the model in the models directory")
}

// Load makes a model resident by taking and immediately releasing a lease: the
// pool keeps it warm afterwards, subject to its eviction policy.
func (m *Models) Load(ctx context.Context, id string) error {
	lease, err := m.pool.Acquire(ctx, id)
	if err != nil {
		return err
	}
	lease.Release()
	return nil
}

func (m *Models) Unload(_ context.Context, id string) error { return m.pool.Unload(id) }

func (m *Models) Pin(_ context.Context, id string, pinned bool) error {
	return m.pool.Pin(id, pinned)
}

func (m *Models) Reload(ctx context.Context, id, revision string) error {
	return m.pool.Reload(ctx, id, revision)
}

// describe projects a manifest for a model that is not loaded. Capabilities
// come from the family, which is the same source the loaded recogniser reports,
// so the two views cannot disagree.
func describe(man registry.Manifest, state core.ModelState) core.ModelInfo {
	var caps core.Capabilities
	if fam, err := sherpa.LookupFamily(man.Family); err == nil {
		caps = fam.Capabilities()
	}
	return core.ModelInfo{
		ID:           man.ID,
		Revision:     man.Revision,
		DisplayName:  man.DisplayName,
		Family:       man.Family,
		Languages:    man.Languages,
		License:      man.License,
		State:        state,
		RSSMB:        man.Resources.ApproxRSSMB,
		Capabilities: caps,
	}
}

var _ core.ModelService = (*Models)(nil)
