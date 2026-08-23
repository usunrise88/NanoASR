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
type Loader func(ctx context.Context, m registry.Manifest, dir string) (asr.Recognizer, error)

// Options configures the pool. Zero values are replaced with safe minimums.
type Options struct {
	MaxResidentModels int
	MaxModelRSSMB     int
	IdleTTL           time.Duration
	AcquireTimeout    time.Duration
	Observer          core.Observer
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
	id       string
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
	ctx, cancel := context.WithTimeout(ctx, p.opt.AcquireTimeout)
	defer cancel()

	for {
		p.mu.Lock()
		e, ok := p.current[id]
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

		// Nothing serving this id: claim the slot and load it ourselves.
		placeholder := &entry{
			id:       id,
			state:    core.ModelLoading,
			ready:    make(chan struct{}),
			lastUsed: time.Now(),
		}
		p.current[id] = placeholder
		p.mu.Unlock()

		p.opt.Observer.ModelEvent(id, core.ModelAbsent, core.ModelLoading)
		rec, man, err := p.materialise(ctx, id)

		p.mu.Lock()
		if err != nil {
			placeholder.loadErr = err
			delete(p.current, id)
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
func (p *Pool) materialise(ctx context.Context, id string) (asr.Recognizer, registry.Manifest, error) {
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
	rec, err := p.load(ctx, man, dir)
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
	rec, man, err := p.materialise(ctx, id)
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
		manifest: man,
		rec:      rec,
		state:    core.ModelReady,
		rssMB:    man.Resources.ApproxRSSMB,
		lastUsed: time.Now(),
		ready:    closedChan(),
	}
	if old, ok := p.current[id]; ok {
		fresh.pinned = old.pinned
		old.state = core.ModelDraining
		if old.refs == 0 {
			p.closeLocked(old)
		} else {
			p.draining = append(p.draining, old)
		}
		p.opt.Observer.ModelEvent(id, core.ModelReady, core.ModelDraining)
	}
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
	delete(p.current, id)
	if e.refs == 0 {
		p.closeLocked(e)
		return nil
	}
	e.state = core.ModelDraining
	p.draining = append(p.draining, e)
	return nil
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
	sort.Slice(cands, func(i, j int) bool { return cands[i].lastUsed.Before(cands[j].lastUsed) })
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
			ID:           e.id,
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
