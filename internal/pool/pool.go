package pool

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// Loader turns a resolved manifest into a live recogniser. It is injected so
// the pool can be tested without models, and so an out-of-process backend
// (SPEC §17, M6) can replace it without touching this file.
type Loader func(ctx context.Context, m registry.Manifest, dir string, v asr.Variant) (asr.Recognizer, error)

// Options configures the pool. Zero values are replaced with safe minimums.
type Options struct {
	MaxResidentModels int
	MaxModelRSSMB     int
	IdleTTL           time.Duration
	AcquireTimeout    time.Duration
	Observer          core.Observer
	// MaxVariants caps the extra instances one model may hold for per-request
	// configurations. Zero — the default — means the base instance is the only
	// one, and a request asking for a variant is answered with the model's own
	// settings plus a warning saying so.
	MaxVariants int
}

// Pool keeps loaded models alive, bounded by count and by memory, and performs
// hot swaps without interrupting in-flight work (SPEC §7.4).
//
// The invariant that makes hot swap safe: an entry with refs > 0 is never
// unloaded. A swap installs a new entry beside the old one and moves the old
// one to draining; the last Release closes it.
type Pool struct {
	mu   sync.Mutex
	cond *sync.Cond

	current  map[string]*entry // model id → serving entry
	draining []*entry

	reg  registry.Registry
	load Loader
	opt  Options
}

type entry struct {
	// id is the pool key: the model id for the base instance, and the model id
	// with a variant suffix for the others. baseID is the model either way, so
	// a variant can be counted against its model and reported under it.
	id       string
	baseID   string
	variant  asr.Variant
	manifest registry.Manifest
	rec      asr.Recognizer

	state    core.ModelState
	refs     int
	pinned   bool
	rssMB    int
	lastUsed time.Time

	ready   chan struct{} // closed once loading finished (successfully or not)
	loadErr error
}

// New builds a pool. reg resolves ids, load materialises recognisers.
func New(reg registry.Registry, load Loader, opt Options) *Pool {
	if opt.MaxResidentModels < 1 {
		opt.MaxResidentModels = 1
	}
	if opt.MaxModelRSSMB < 1 {
		opt.MaxModelRSSMB = 1024
	}
	if opt.AcquireTimeout <= 0 {
		opt.AcquireTimeout = 30 * time.Second
	}
	if opt.Observer == nil {
		opt.Observer = core.NopObserver{}
	}
	p := &Pool{current: map[string]*entry{}, reg: reg, load: load, opt: opt}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Lease is a borrowed model. Release exactly once; the pool will not unload the
// model while any lease is outstanding.
type Lease struct {
	Recognizer asr.Recognizer
	Manifest   registry.Manifest

	pool *Pool
	e    *entry
	once sync.Once
}

// Release returns the lease.
func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() { l.pool.release(l.e) })
}

// Acquire returns a ready model, loading or waiting as needed. It fails with
// model_unavailable rather than blocking forever when nothing can be evicted.
func (p *Pool) Acquire(ctx context.Context, id string) (*Lease, error) {
	return p.AcquireVariant(ctx, id, asr.Variant{})
}

// AcquireVariant is Acquire for a request that needs its own recogniser
// configuration. The zero variant is the base instance, so the two share a key
// and ordinary traffic never pays for the feature.
func (p *Pool) AcquireVariant(ctx context.Context, id string, v asr.Variant) (*Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, p.opt.AcquireTimeout)
	defer cancel()

	key := variantKey(id, v)
	if !v.Zero() {
		if err := p.admitVariant(id, key); err != nil {
			return nil, err
		}
	}

	for {
		p.mu.Lock()
		e, ok := p.current[key]
		if ok {
			switch e.state {
			case core.ModelReady:
				e.refs++
				e.lastUsed = time.Now()
				p.mu.Unlock()
				return &Lease{Recognizer: e.rec, Manifest: e.manifest, pool: p, e: e}, nil
			case core.ModelLoading, core.ModelDownloading:
				ready := e.ready
				p.mu.Unlock()
				select {
				case <-ready:
					continue // re-check: it may have failed or been swapped
				case <-ctx.Done():
					return nil, timeoutErr(ctx, id)
				}
			}
		}

		// Nothing serving this key: claim the slot and load it ourselves.
		placeholder := &entry{
			id:       key,
			baseID:   id,
			variant:  v,
			state:    core.ModelLoading,
			ready:    make(chan struct{}),
			lastUsed: time.Now(),
		}
		p.current[key] = placeholder
		p.mu.Unlock()

		p.opt.Observer.ModelEvent(id, core.ModelAbsent, core.ModelLoading)
		rec, man, err := p.materialise(ctx, id, v)

		p.mu.Lock()
		if err != nil {
			placeholder.loadErr = err
			delete(p.current, key)
			close(placeholder.ready)
			p.cond.Broadcast()
			p.mu.Unlock()
			return nil, err
		}
		placeholder.rec = rec
		placeholder.manifest = man
		placeholder.rssMB = man.Resources.ApproxRSSMB
		placeholder.state = core.ModelReady
		placeholder.refs = 1
		placeholder.lastUsed = time.Now()
		close(placeholder.ready)

		p.evictLocked()
		p.mu.Unlock()

		p.opt.Observer.ModelEvent(id, core.ModelLoading, core.ModelReady)
		return &Lease{Recognizer: rec, Manifest: man, pool: p, e: placeholder}, nil
	}
}

// materialise resolves, downloads if needed and loads. It runs without the
// pool lock held: loading a model takes seconds and must not block Acquire for
// every other model.
func (p *Pool) materialise(ctx context.Context, id string, v asr.Variant) (asr.Recognizer, registry.Manifest, error) {
	man, err := p.reg.Resolve(ctx, id)
	if err != nil {
		return nil, registry.Manifest{}, err
	}
	// Ensure blocks until the model is on disk, so a download failure arrives
	// here as an error rather than as a missing file two lines later.
	dir, err := p.reg.Ensure(ctx, id)
	if err != nil {
		return nil, man, err
	}
	rec, err := p.load(ctx, man, dir, v)
	if err != nil {
		return nil, man, err
	}
	return rec, man, nil
}

func (p *Pool) release(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	e.refs--
	if e.refs < 0 {
		e.refs = 0
	}
	e.lastUsed = time.Now()

	if e.state == core.ModelDraining && e.refs == 0 {
		p.closeLocked(e)
		p.removeDrainingLocked(e)
	}
	p.cond.Broadcast()
}

// Reload performs a hot swap: load the new revision beside the old one and
// switch only once it is ready. In-flight requests keep decoding on the old
// instance until they finish, and nobody ever sees a half-loaded model.
func (p *Pool) Reload(ctx context.Context, id, revision string) error {
	rec, man, err := p.materialise(ctx, id, asr.Variant{})
	if err != nil {
		return err
	}
	if revision != "" && man.Revision != revision {
		_ = rec.Close()
		return core.Errorf(core.CodeModelNotFound,
			"model %s: resolved revision %q, wanted %q", id, man.Revision, revision)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	fresh := &entry{
		id:       id,
		baseID:   id,
		manifest: man,
		rec:      rec,
		state:    core.ModelReady,
		rssMB:    man.Resources.ApproxRSSMB,
		lastUsed: time.Now(),
		ready:    closedChan(),
	}
	if old, ok := p.current[id]; ok {
		fresh.pinned = old.pinned
		p.retireLocked(old)
		p.opt.Observer.ModelEvent(id, core.ModelReady, core.ModelDraining)
	}
	// Every variant of this model was built from the revision being replaced.
	// Leaving them would mean a request with hotwords quietly kept decoding on
	// weights the hot swap was meant to retire.
	p.retireVariantsLocked(id)
	p.current[id] = fresh
	p.evictLocked()
	p.cond.Broadcast()
	return nil
}

// Unload removes a model once nothing is using it.
func (p *Pool) Unload(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.current[id]
	if !ok {
		return core.Errorf(core.CodeModelNotFound, "model %s is not loaded", id)
	}
	p.retireLocked(e)
	p.retireVariantsLocked(id)
	return nil
}

// retireLocked takes an entry out of service, closing it now if nothing holds
// it and draining it otherwise.
func (p *Pool) retireLocked(e *entry) {
	delete(p.current, e.id)
	if e.refs == 0 {
		p.closeLocked(e)
		return
	}
	e.state = core.ModelDraining
	p.draining = append(p.draining, e)
}

// retireVariantsLocked retires every variant instance of a base model.
func (p *Pool) retireVariantsLocked(baseID string) {
	for _, e := range p.current {
		if e.baseID == baseID && e.id != baseID {
			p.retireLocked(e)
		}
	}
}

// Pin protects a model from eviction.
func (p *Pool) Pin(id string, pinned bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.current[id]
	if !ok {
		return core.Errorf(core.CodeModelNotFound, "model %s is not loaded", id)
	}
	e.pinned = pinned
	return nil
}

// Sweep unloads models idle for longer than IdleTTL. Call it periodically.
func (p *Pool) Sweep(now time.Time) int {
	if p.opt.IdleTTL <= 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for id, e := range p.current {
		if e.pinned || e.refs > 0 || e.state != core.ModelReady {
			continue
		}
		if now.Sub(e.lastUsed) > p.opt.IdleTTL {
			delete(p.current, id)
			p.closeLocked(e)
			n++
		}
	}
	return n
}

// evictLocked drops least-recently-used idle models until the pool fits both
// limits. Pinned models and models with outstanding leases are never candidates,
// so the pool can legitimately sit over its limit while everything is in use.
func (p *Pool) evictLocked() {
	for p.overLimitLocked() {
		victim := p.lruVictimLocked()
		if victim == nil {
			return
		}
		delete(p.current, victim.id)
		p.closeLocked(victim)
		p.opt.Observer.ModelEvent(victim.id, core.ModelReady, core.ModelAbsent)
	}
}

func (p *Pool) overLimitLocked() bool {
	if len(p.current) > p.opt.MaxResidentModels {
		return true
	}
	total := 0
	for _, e := range p.current {
		total += e.rssMB
	}
	return total > p.opt.MaxModelRSSMB
}

func (p *Pool) lruVictimLocked() *entry {
	cands := make([]*entry, 0, len(p.current))
	for _, e := range p.current {
		if e.pinned || e.refs > 0 || e.state != core.ModelReady {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil
	}
	// A variant goes before a base instance of the same age. A base model is
	// what ordinary requests use; a variant serves whoever asked for hotwords
	// and can be rebuilt from it.
	sort.Slice(cands, func(i, j int) bool {
		iv, jv := cands[i].id != cands[i].baseID, cands[j].id != cands[j].baseID
		if iv != jv {
			return iv
		}
		return cands[i].lastUsed.Before(cands[j].lastUsed)
	})
	return cands[0]
}

func (p *Pool) closeLocked(e *entry) {
	if e.rec != nil {
		_ = e.rec.Close()
		e.rec = nil
	}
}

func (p *Pool) removeDrainingLocked(e *entry) {
	for i, d := range p.draining {
		if d == e {
			p.draining = append(p.draining[:i], p.draining[i+1:]...)
			return
		}
	}
}

// List reports what the pool is holding, for the models API and the UI.
func (p *Pool) List() []core.ModelInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.ModelInfo, 0, len(p.current))
	for _, e := range p.current {
		// Capabilities come from the loaded recogniser rather than the
		// manifest, so what is reported is what the model actually does.
		var caps core.Capabilities
		if e.rec != nil {
			caps = e.rec.Capabilities()
		}
		out = append(out, core.ModelInfo{
			ID:           e.baseID,
			Variant:      e.variant.String(),
			Revision:     e.manifest.Revision,
			Kind:         e.manifest.EffectiveKind(),
			DisplayName:  e.manifest.DisplayName,
			Family:       e.manifest.Family,
			Languages:    e.manifest.Languages,
			License:      e.manifest.License,
			State:        e.state,
			Pinned:       e.pinned,
			RefCount:     e.refs,
			RSSMB:        e.rssMB,
			LastUsedUnix: e.lastUsed.Unix(),
			Capabilities: caps,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close unloads everything. In-flight leases are not waited for: callers must
// drain the queue before shutting the pool down.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.current {
		p.closeLocked(e)
		delete(p.current, id)
	}
	for _, e := range p.draining {
		p.closeLocked(e)
	}
	p.draining = nil
	return nil
}

func timeoutErr(ctx context.Context, id string) error {
	if ctx.Err() == context.DeadlineExceeded {
		return core.Errorf(core.CodeModelUnavailable,
			"timed out waiting for model %s to become available", id)
	}
	return ctx.Err()
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// variantKey is the map key for one configuration of one model. The zero
// variant keys on the model id alone, so ordinary traffic is unaffected by the
// feature existing.
func variantKey(id string, v asr.Variant) string {
	if v.Zero() {
		return id
	}
	return id + "!" + v.Key()
}

// admitVariant decides whether a second instance of a model may be loaded.
//
// The budget is per base model rather than global: one model with a runaway
// client must not evict every other model's variants, and a cap that counted
// them together would let it. Hitting the cap warns rather than evicting,
// because evicting to make room for a variant means a client that sends a fresh
// hotword list every request can churn the pool indefinitely.
func (p *Pool) admitVariant(baseID, key string) error {
	if p.opt.MaxVariants <= 0 {
		return core.Errorf(core.CodeCapabilityUnavailable,
			"per-request recogniser settings need a second resident instance, "+
				"and asr.variants.max is 0")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for _, e := range p.current {
		if e.baseID != baseID || e.id == baseID {
			continue
		}
		if e.id == key {
			return nil // already resident: reusing it costs nothing
		}
		n++
	}
	if n >= p.opt.MaxVariants {
		return core.Errorf(core.CodeCapabilityUnavailable,
			"model %s already holds %d recogniser variants (asr.variants.max)", baseID, n)
	}
	return nil
}

// Manifest resolves a model's manifest without loading it.
//
// The pipeline needs it to decide whether a request's options are expressible
// on this model at all, and that decision has to be made before committing to
// the memory of a second instance.
func (p *Pool) Manifest(ctx context.Context, id string) (registry.Manifest, error) {
	p.mu.Lock()
	if e, ok := p.current[id]; ok && e.state == core.ModelReady {
		man := e.manifest
		p.mu.Unlock()
		return man, nil
	}
	p.mu.Unlock()
	return p.reg.Resolve(ctx, id)
}
