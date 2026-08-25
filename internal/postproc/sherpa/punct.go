// Package sherpa binds internal/postproc to sherpa-onnx's OfflinePunctuation.
//
// Separate from internal/postproc for the same reason as everywhere else in
// this tree: the re-attachment logic is string arithmetic and is tested without
// cgo or weights, and only this file needs a model on disk.
package sherpa

import (
	"context"
	"runtime"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Punctuator is one CT-Transformer instance.
type Punctuator struct {
	mu     sync.Mutex
	impl   *sonnx.OfflinePunctuation
	closed bool
}

// New loads a punctuation model.
func New(modelPath string, numThreads int) (*Punctuator, error) {
	if modelPath == "" {
		return nil, core.Errorf(core.CodeInvalidRequest, "punctuation: no model path configured")
	}
	if numThreads < 1 {
		numThreads = 1
	}

	impl := sonnx.NewOfflinePunctuation(&sonnx.OfflinePunctuationConfig{
		Model: sonnx.OfflinePunctuationModelConfig{
			CtTransformer: modelPath,
			NumThreads:    numThreads,
			Provider:      "cpu",
		},
	})
	if impl == nil {
		return nil, core.Errorf(core.CodeInternal,
			"sherpa-onnx refused to create the punctuation model")
	}
	p := &Punctuator{impl: impl}
	runtime.SetFinalizer(p, func(p *Punctuator) { _ = p.Close() })
	return p, nil
}

// AddPunct restores marks and capitals.
//
// The lock is not optimism about the binding's thread safety — it makes none —
// and concurrency comes from the pool holding several instances instead.
func (p *Punctuator) AddPunct(ctx context.Context, text string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if text == "" {
		return "", nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return "", core.Errorf(core.CodeInternal, "punctuation model is closed")
	}
	return p.impl.AddPunct(text), nil
}

func (p *Punctuator) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	// The binding names this DeleteOfflinePunc, not DeleteOfflinePunctuation.
	sonnx.DeleteOfflinePunc(p.impl)
	p.impl = nil
	runtime.SetFinalizer(p, nil)
	return nil
}

// Pool hands out punctuators to concurrent jobs, like vad.Pool.
type Pool struct {
	free chan *Punctuator
	all  []*Punctuator
}

// NewPool builds size instances up front so a bad model path fails at startup
// rather than on the first request that asks for punctuation.
func NewPool(modelPath string, numThreads, size int) (*Pool, error) {
	if size < 1 {
		size = 1
	}
	p := &Pool{free: make(chan *Punctuator, size)}
	for range size {
		one, err := New(modelPath, numThreads)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.all = append(p.all, one)
		p.free <- one
	}
	return p, nil
}

func (p *Pool) AddPunct(ctx context.Context, text string) (string, error) {
	select {
	case one := <-p.free:
		defer func() { p.free <- one }()
		return one.AddPunct(ctx, text)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (p *Pool) Close() error {
	for _, one := range p.all {
		_ = one.Close()
	}
	p.all = nil
	return nil
}
