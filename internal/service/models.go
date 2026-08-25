// Package service implements the administrative surface that sits above the
// registry and the model pool.
package service

import (
	"context"
	"sort"

	"github.com/usunrise88/nanoasr/internal/asr"
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
		// On disk but not in the pool is ModelDownloaded, not ModelAbsent:
		// absent is the top of the download → load → ready ladder and means
		// the weights are not here at all, which is what the catalog reports.
		// Conflating the two told the UI that every installed model was
		// missing.
		out = append(out, m.describe(man, core.ModelDownloaded))
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
		out = append(out, m.describe(man, core.ModelAbsent))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Download starts a fetch, or joins one already running, and streams progress.
// Whether downloading is permitted at all is the registry's decision.
func (m *Models) Download(ctx context.Context, id string) (<-chan core.DownloadProgress, error) {
	return m.registry.Fetch(ctx, id)
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
// describe reports a model that is not resident. Capabilities come from the
// family, plus the one thing the family cannot know.
//
// Whether a model punctuates is a property of its weights, not of its family —
// two GigaAM releases share a family and differ on exactly this — so it is read
// from the vocabulary. It has to be answered here rather than only for loaded
// models, because the UI gates its punctuation switch on this while the user is
// choosing what to run, which is before anything has been loaded.
func (m *Models) describe(man registry.Manifest, state core.ModelState) core.ModelInfo {
	var caps core.Capabilities
	if fam, err := sherpa.LookupFamily(man.Family); err == nil {
		caps = fam.Capabilities()
	}
	if state != core.ModelAbsent {
		caps.PunctuationBuiltin = m.vocabularyPunctuates(man)
	}
	return core.ModelInfo{
		ID:           man.ID,
		Revision:     man.Revision,
		Kind:         man.EffectiveKind(),
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

// vocabularyPunctuates reads a model's tokens file to see whether it can write
// sentence marks and capitals.
//
// Anything unreadable answers false. A model whose weights cannot be opened is
// not usable anyway, and claiming a capability for it would be exactly the kind
// of unchecked claim the manifest deliberately refuses to carry.
func (m *Models) vocabularyPunctuates(man registry.Manifest) bool {
	if man.EffectiveKind() != registry.KindASR {
		return false
	}
	dir, err := m.registry.Dir(man.ID)
	if err != nil {
		return false
	}
	tokens, err := man.FilePath(dir, "tokens")
	if err != nil {
		return false
	}
	return asr.VocabularyPunctuates(tokens)
}
