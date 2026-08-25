package pool

import (
	"context"
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
