package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

type fakeRec struct {
	id     string
	closed bool
	mu     sync.Mutex
}

func (f *fakeRec) Decode(context.Context, [][]float32, int) ([]asr.Recognition, error) {
	return nil, nil
}
func (f *fakeRec) Capabilities() core.Capabilities { return core.Capabilities{} }
func (f *fakeRec) ModelingUnit() string            { return "bpe" }
func (f *fakeRec) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *fakeRec) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type fakeReg struct{ rev string }

func (r fakeReg) Resolve(_ context.Context, id string) (registry.Manifest, error) {
	return registry.Manifest{
		ID: id, Revision: r.rev, Family: "transducer", SampleRate: 16000,
		Resources: registry.Resources{ApproxRSSMB: 1000},
	}, nil
}
func (r fakeReg) Local(context.Context) ([]registry.Manifest, error)   { return nil, nil }
func (r fakeReg) Catalog(context.Context) ([]registry.Manifest, error) { return nil, nil }
func (r fakeReg) Ensure(context.Context, string) (string, error)       { return "/tmp/models", nil }
func (r fakeReg) Fetch(context.Context, string) (<-chan core.DownloadProgress, error) {
	return nil, nil
}
func (r fakeReg) Dir(string) (string, error) { return "/tmp/models", nil }

func newTestPool(t *testing.T, opt Options) (*Pool, map[string]*fakeRec) {
	t.Helper()
	recs := map[string]*fakeRec{}
	var mu sync.Mutex
	load := func(_ context.Context, m registry.Manifest, _ string, _ asr.Variant) (asr.Recognizer, error) {
		mu.Lock()
		defer mu.Unlock()
		r := &fakeRec{id: m.Key()}
		recs[m.Key()] = r
		return r, nil
	}
	p := New(fakeReg{rev: "1"}, load, opt)
	t.Cleanup(func() { _ = p.Close() })
	return p, recs
}

func TestAcquireReusesLoadedModel(t *testing.T) {
	p, recs := newTestPool(t, Options{MaxResidentModels: 2, MaxModelRSSMB: 4000})

	l1, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	l2, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if l1.Recognizer != l2.Recognizer {
		t.Fatal("second Acquire loaded a second instance of the same model")
	}
	if len(recs) != 1 {
		t.Fatalf("loaded %d instances, want 1", len(recs))
	}
	l1.Release()
	l2.Release()
}

func TestEvictionSkipsLeasedAndPinned(t *testing.T) {
	p, recs := newTestPool(t, Options{MaxResidentModels: 1, MaxModelRSSMB: 100000})

	// "a" stays leased, so loading "b" must not evict it even though the pool
	// is over its model-count limit.
	la, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	lb, err := p.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if recs["a@1"].isClosed() {
		t.Fatal("evicted a model that still had an outstanding lease")
	}

	// Once released, "a" becomes the LRU victim when "c" arrives.
	la.Release()
	lb.Release()
	lc, err := p.Acquire(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Release()
	if !recs["a@1"].isClosed() && !recs["b@1"].isClosed() {
		t.Fatal("nothing was evicted although the pool is over its limit")
	}
}

func TestReleaseEvictsWhenOverLimit(t *testing.T) {
	p, recs := newTestPool(t, Options{MaxResidentModels: 2, MaxModelRSSMB: 100000})

	la, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	lb, err := p.Acquire(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	// The pool is at its model-count limit; loading "c" pushes it over.
	lc, err := p.Acquire(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Release()

	// Once "a" goes idle the pool must reclaim it right away, without
	// waiting for another load.
	la.Release()
	if !recs["a@1"].isClosed() {
		t.Fatal("releasing the LRU did not trigger eviction")
	}
	lb.Release()
}

func TestReloadKeepsInFlightRequestAlive(t *testing.T) {
	p, recs := newTestPool(t, Options{MaxResidentModels: 4, MaxModelRSSMB: 100000})

	lease, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	old := lease.Recognizer

	if err := p.Reload(context.Background(), "a", ""); err != nil {
		t.Fatal(err)
	}
	// The swap must not pull the recogniser out from under an active request.
	if old.(*fakeRec).isClosed() {
		t.Fatal("hot swap closed a recogniser that a request was still using")
	}

	// A new request gets the new instance.
	fresh, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	if fresh.Recognizer == old {
		t.Fatal("after Reload a new lease still points at the old instance")
	}

	// Releasing the last lease on the drained instance closes it.
	lease.Release()
	if !old.(*fakeRec).isClosed() {
		t.Fatal("drained instance was not closed after its last lease was released")
	}
	_ = recs
}

func TestSweepUnloadsIdleModels(t *testing.T) {
	p, recs := newTestPool(t, Options{MaxResidentModels: 4, MaxModelRSSMB: 100000, IdleTTL: time.Minute})

	l, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	l.Release()

	if n := p.Sweep(time.Now()); n != 0 {
		t.Fatalf("swept %d models that were not idle long enough", n)
	}
	if n := p.Sweep(time.Now().Add(2 * time.Minute)); n != 1 {
		t.Fatalf("swept %d models, want 1", n)
	}
	if !recs["a@1"].isClosed() {
		t.Fatal("swept model was not closed")
	}
}

// gatedLoader lets a test decide when each load finishes and with what
// outcome, so the interleavings between an in-flight load and Reload/Unload
// can be made deterministic.
type gatedLoader struct {
	mu      sync.Mutex
	started chan int
	gates   []chan error
	recs    []*fakeRec
}

func newGatedLoader() *gatedLoader {
	return &gatedLoader{started: make(chan int)}
}

func (g *gatedLoader) load(_ context.Context, m registry.Manifest, _ string, _ asr.Variant) (asr.Recognizer, error) {
	g.mu.Lock()
	idx := len(g.gates)
	gate := make(chan error, 1)
	g.gates = append(g.gates, gate)
	r := &fakeRec{id: m.Key()}
	g.recs = append(g.recs, r)
	g.mu.Unlock()

	g.started <- idx
	if err := <-gate; err != nil {
		return nil, err
	}
	return r, nil
}

func (g *gatedLoader) finish(idx int, err error) { g.gates[idx] <- err }

func (g *gatedLoader) rec(idx int) *fakeRec {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.recs[idx]
}

func TestUnloadDuringLoadDoesNotLeakOrResurrect(t *testing.T) {
	g := newGatedLoader()
	p := New(fakeReg{rev: "1"}, g.load, Options{MaxResidentModels: 2, MaxModelRSSMB: 4000})
	defer func() { _ = p.Close() }()

	acquireDone := make(chan error, 1)
	go func() {
		l, err := p.Acquire(context.Background(), "a")
		if l != nil {
			l.Release()
		}
		acquireDone <- err
	}()

	<-g.started // the load is in flight, the placeholder is serving as "a"
	if err := p.Unload("a"); err != nil {
		t.Fatalf("Unload of a loading model: %v", err)
	}
	g.finish(0, nil)

	if err := <-acquireDone; err == nil {
		t.Fatal("Acquire succeeded although the model was unloaded mid-load")
	}
	if !g.rec(0).isClosed() {
		t.Fatal("the recogniser built for a retired placeholder was never closed")
	}
	if got := len(p.List()); got != 0 {
		t.Fatalf("pool lists %d models after unload-during-load, want 0", got)
	}
}

func TestReloadDuringLoadServesTheFreshInstance(t *testing.T) {
	g := newGatedLoader()
	p := New(fakeReg{rev: "1"}, g.load, Options{MaxResidentModels: 4, MaxModelRSSMB: 100000})
	defer func() { _ = p.Close() }()

	type acquired struct {
		lease *Lease
		err   error
	}
	acquireDone := make(chan acquired, 1)
	go func() {
		l, err := p.Acquire(context.Background(), "a")
		acquireDone <- acquired{l, err}
	}()

	<-g.started // call 0: the Acquire's load is in flight
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- p.Reload(context.Background(), "a", "") }()
	<-g.started // call 1: the Reload's load is in flight

	// The Reload finishes first and retires the Acquire's placeholder.
	g.finish(1, nil)
	if err := <-reloadDone; err != nil {
		t.Fatalf("Reload: %v", err)
	}
	g.finish(0, nil)

	a := <-acquireDone
	if a.err != nil {
		t.Fatalf("Acquire: %v", a.err)
	}
	defer a.lease.Release()
	if a.lease.Recognizer != g.rec(1) {
		t.Fatal("Acquire did not land on the instance Reload installed")
	}
	if !g.rec(0).isClosed() {
		t.Fatal("the recogniser built for the retired placeholder was never closed")
	}
}

func TestFailedLoadAfterReloadKeepsTheFreshEntry(t *testing.T) {
	g := newGatedLoader()
	p := New(fakeReg{rev: "1"}, g.load, Options{MaxResidentModels: 4, MaxModelRSSMB: 100000})
	defer func() { _ = p.Close() }()

	acquireDone := make(chan error, 1)
	go func() {
		l, err := p.Acquire(context.Background(), "a")
		if l != nil {
			l.Release()
		}
		acquireDone <- err
	}()

	<-g.started // call 0: the Acquire's load is in flight
	reloadDone := make(chan error, 1)
	go func() { reloadDone <- p.Reload(context.Background(), "a", "") }()
	<-g.started // call 1: the Reload's load is in flight

	g.finish(1, nil) // Reload succeeds and installs its entry under "a"
	if err := <-reloadDone; err != nil {
		t.Fatalf("Reload: %v", err)
	}
	g.finish(0, errors.New("weights unreadable")) // the older load now fails

	if err := <-acquireDone; err == nil {
		t.Fatal("Acquire returned nil after its load failed")
	}
	// The failing load must not take the fresh entry down with it.
	l, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("the freshly reloaded model is gone: %v", err)
	}
	defer l.Release()
	if l.Recognizer != g.rec(1) {
		t.Fatal("the entry left behind is not the one Reload installed")
	}
	if g.rec(1).isClosed() {
		t.Fatal("the fresh recogniser was closed by someone else's failed load")
	}
}

func TestCloseWaitsForOutstandingLeases(t *testing.T) {
	p, recs := newTestPool(t, Options{MaxResidentModels: 2, MaxModelRSSMB: 4000})

	l, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}

	// A close that runs while a lease is outstanding must not pull the
	// recogniser out from under the holder, and it must return once the
	// holder releases.
	closeDone := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("Close returned while a lease was still outstanding")
	case <-time.After(50 * time.Millisecond):
	}

	l.Release()

	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after the lease was released")
	}
	if !recs["a@1"].isClosed() {
		t.Fatal("the model was not freed after Close drained the lease")
	}
}

func TestCloseSkipsNativeCloseOnStragglers(t *testing.T) {
	// With a one-millisecond drain timeout, a lease that outlives Close is
	// left for the process to take down; the recogniser is not closed under
	// it, which would crash any handler still decoding.
	p, recs := newTestPool(t, Options{MaxResidentModels: 2, MaxModelRSSMB: 4000})
	p.closeDrainTimeoutForTest = 1 * time.Millisecond

	l, err := p.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	go func() { _ = p.Close(); close(closed) }()
	<-closed

	if recs["a@1"].isClosed() {
		t.Fatal("Close freed a recogniser a handler still holds")
	}
	l.Release()
}

func TestAcquireAfterCloseReturnsAnError(t *testing.T) {
	p, _ := newTestPool(t, Options{MaxResidentModels: 2, MaxModelRSSMB: 4000})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(context.Background(), "a"); err == nil {
		t.Fatal("Acquire after Close returned a lease")
	}
}

func TestGovernorBoundsConcurrency(t *testing.T) {
	g := NewGovernor(4)
	ctx := context.Background()

	if err := g.Acquire(ctx, 3); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Needs 3 more slots than are free; must wait for the release.
		_ = g.Acquire(ctx, 3)
	}()

	select {
	case <-done:
		t.Fatal("governor handed out more slots than its capacity")
	case <-time.After(50 * time.Millisecond):
	}

	g.Release(3)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("governor did not wake a waiter after Release")
	}
	g.Release(3)

	if used, _ := g.Stats(); used != 0 {
		t.Fatalf("used = %d after all releases, want 0", used)
	}
}

func TestGovernorRespectsContextCancellation(t *testing.T) {
	g := NewGovernor(1)
	if err := g.Acquire(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.Acquire(ctx, 1); err == nil {
		t.Fatal("Acquire returned nil after its context expired")
	}
	g.Release(1)
}
