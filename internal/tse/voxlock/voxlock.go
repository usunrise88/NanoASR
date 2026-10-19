// Package voxlock runs Voxlock, a causal band-split RNN that keeps one voice
// out of a recording and removes the others. The voice is named by a 3-second
// sample of it, the enrollment; everything else in the recording is treated as
// somebody else.
//
// The model is two ONNX graphs run through onnxruntime directly (sherpa-onnx
// has no target-speaker extraction): enroll.onnx turns the sample into the
// conditioning the extractor reads, and chunk.onnx turns a stretch of the
// recording into windowed time-domain frames, carrying its LSTM state from one
// stretch to the next. The STFT padding and the overlap-add that turn those
// frames back into audio stay outside the graph, here, ported from the
// reference implementation (voxlock 1.1.1, voxlock/core.py) line for line.
package voxlock

import (
	"context"
	"fmt"
	"math"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/ortenv"
)

const (
	// SampleRate is the only rate the graphs accept.
	SampleRate = 16000
	// EnrollSamples is the exact length of a voice sample. The graphs declare
	// the axis dynamic, but the extractor's attention reshapes the context to
	// the 298 frames of a 3-second sample, and any other length fails inside
	// the graph rather than at its input.
	EnrollSamples = 3 * SampleRate

	hop  = 128
	nfft = 512
	pad  = nfft / 2
	// gain undoes the scale the network was trained into: it returns the
	// voice at about -2x, because a scale-invariant loss does not care.
	gain = -0.5
	// framesPerRun bounds one chunk.onnx call: 100 frames are 0.8 s of audio,
	// small enough to notice a cancellation between calls and large enough
	// that the per-call overhead does not show.
	framesPerRun = 100

	lstmLayers = 6
	lstmBatch  = 32
	lstmHidden = 256
	stateSize  = lstmLayers * lstmBatch * lstmHidden
)

// win2 is the squared periodic Hann window: the envelope one frame adds to the
// overlap-add, since the graph applies the window on analysis and synthesis.
var win2 = func() [nfft]float32 {
	var w [nfft]float32
	for i := range w {
		h := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/nfft)
		w[i] = float32(h * h)
	}
	return w
}()

// Files are the model's graphs on disk.
type Files struct {
	Enroll string // enroll.onnx
	Chunk  string // chunk_int8.onnx or another precision of chunk.onnx
}

// Voice is an enrolled voice: what the extractor is conditioned on. It is
// immutable and may be shared by any number of streams.
type Voice struct {
	mag, ctx       []float32
	magDim, ctxDim [3]int64
}

// graphs is the boundary to onnxruntime, so the signal path around it can be
// tested without the weights.
type graphs interface {
	enroll(sample []float32) (*Voice, error)
	// chunk extracts k frames from wav, which holds exactly (k-1)*hop+nfft
	// samples, and replaces h and c with the state after them.
	chunk(wav []float32, k int, v *Voice, h, c []float32) ([]float32, error)
	close() error
}

// Model is a loaded Voxlock. Its sessions are safe for concurrent use; the
// state of a recording lives in its Stream.
type Model struct {
	g graphs
}

// Load opens both graphs with the given thread budget.
func Load(f Files, tune ortenv.Tuning) (*Model, error) {
	g, err := newORTGraphs(f, tune)
	if err != nil {
		return nil, err
	}
	return &Model{g: g}, nil
}

// Close releases the sessions. Streams of this model must not be used after.
func (m *Model) Close() error { return m.g.close() }

// Enroll conditions the model on a voice. The sample must be exactly
// EnrollSamples long, at SampleRate.
func (m *Model) Enroll(sample []float32) (*Voice, error) {
	if len(sample) != EnrollSamples {
		return nil, core.Errorf(core.CodeInternal,
			"voxlock: a voice sample is %d samples, got %d", EnrollSamples, len(sample))
	}
	return m.g.enroll(sample)
}

// Stream isolates one recording, pushed in pieces of any size. It is not safe
// for concurrent use.
type Stream struct {
	g graphs
	v *Voice

	h, c []float32
	// head holds the first samples until there are enough to build the
	// reflected padding the reference STFT puts before the signal.
	head    []float32
	started bool
	// buf is padded input not yet consumed by a frame.
	buf []float32
	// ola and env are the overlap-add of the frames so far and the window
	// envelope that normalises it, from the first sample a later frame can
	// still touch.
	ola, env []float32
	// dropped counts the leading padding still to cut from the output.
	dropped int
}

// NewStream starts a recording isolated to v.
func (m *Model) NewStream(v *Voice) *Stream {
	return &Stream{
		g:   m.g,
		v:   v,
		h:   make([]float32, stateSize),
		c:   make([]float32, stateSize),
		ola: make([]float32, nfft),
		env: make([]float32, nfft),
	}
}

// Push takes the next samples and returns the isolated audio that is ready,
// which runs about 24 ms behind the input.
func (s *Stream) Push(chunk []float32) ([]float32, error) {
	if !s.started {
		s.head = append(s.head, chunk...)
		if len(s.head) <= pad {
			return nil, nil
		}
		// torch.stft's centre padding: the first pad samples mirrored
		// around the first one, which is not repeated.
		s.buf = make([]float32, 0, pad+len(s.head))
		for i := pad; i >= 1; i-- {
			s.buf = append(s.buf, s.head[i])
		}
		s.buf = append(s.buf, s.head...)
		s.head, s.started, s.dropped = nil, true, pad
	} else {
		s.buf = append(s.buf, chunk...)
	}
	return s.run()
}

// Flush ends the recording and returns the samples still held back.
func (s *Stream) Flush() ([]float32, error) {
	if !s.started {
		if len(s.head) == 0 {
			return nil, nil
		}
		// Too short for the reference's padding: mirror what there is, so
		// a recording of a few milliseconds still comes back the right
		// length rather than empty.
		s.buf = append([]float32(nil), s.head...)
		s.head, s.started = nil, true
	}
	// The end padding mirrors the last pad samples before the final one.
	n := len(s.buf)
	for i := n - 2; i >= n-pad-1 && i >= 0; i-- {
		s.buf = append(s.buf, s.buf[i])
	}
	frames := 1
	if len(s.buf) > nfft {
		frames = 1 + (len(s.buf)-nfft)/hop
	}
	if need := (frames-1)*hop + nfft; len(s.buf) < need {
		s.buf = append(s.buf, make([]float32, need-len(s.buf))...)
	}
	out, err := s.run()
	if err != nil {
		return nil, err
	}
	out = append(out, s.normalise(len(s.ola))...)
	s.buf = nil
	return out, nil
}

func (s *Stream) run() ([]float32, error) {
	var out []float32
	for len(s.buf) >= nfft {
		k := min(framesPerRun, 1+(len(s.buf)-nfft)/hop)
		fr, err := s.g.chunk(s.buf[:(k-1)*hop+nfft], k, s.v, s.h, s.c)
		if err != nil {
			return nil, err
		}
		s.ola = append(s.ola, make([]float32, k*hop)...)
		s.env = append(s.env, make([]float32, k*hop)...)
		for j := range k {
			o, e, f := s.ola[j*hop:j*hop+nfft], s.env[j*hop:j*hop+nfft], fr[j*nfft:(j+1)*nfft]
			for i := range nfft {
				o[i] += f[i]
				e[i] += win2[i]
			}
		}
		// Samples no later frame will touch.
		done := k * hop
		y := s.normalise(done)
		s.ola = append(s.ola[:0], s.ola[done:]...)
		s.env = append(s.env[:0], s.env[done:]...)
		s.buf = s.buf[done:]
		cut := min(s.dropped, len(y))
		s.dropped -= cut
		out = append(out, y[cut:]...)
	}
	return out, nil
}

// normalise returns the first n overlap-added samples, divided by their window
// envelope and scaled back to the input level.
func (s *Stream) normalise(n int) []float32 {
	y := make([]float32, n)
	for i := range y {
		e := s.env[i]
		if e <= 1e-11 {
			e = 1
		}
		y[i] = s.ola[i] / e * gain
	}
	return y
}

// Isolate runs a whole recording and returns the isolated audio, exactly as
// long as the input. progress, when set, is told how many input samples have
// been processed; ctx is checked between graph calls.
func (m *Model) Isolate(ctx context.Context, audio []float32, v *Voice, progress func(done int)) ([]float32, error) {
	s := m.NewStream(v)
	out := make([]float32, 0, len(audio)+nfft)
	const step = framesPerRun * hop
	for at := 0; at < len(audio); at += step {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		y, err := s.Push(audio[at:min(at+step, len(audio))])
		if err != nil {
			return nil, err
		}
		out = append(out, y...)
		if progress != nil {
			progress(min(at+step, len(audio)))
		}
	}
	y, err := s.Flush()
	if err != nil {
		return nil, err
	}
	out = append(out, y...)
	if len(out) < len(audio) {
		out = append(out, make([]float32, len(audio)-len(out))...)
	}
	return out[:len(audio)], nil
}

// ortGraphs is graphs on onnxruntime.
type ortGraphs struct {
	enrollSess, chunkSess *ort.DynamicAdvancedSession
}

func newORTGraphs(f Files, tune ortenv.Tuning) (*ortGraphs, error) {
	if err := ortenv.Ensure(); err != nil {
		return nil, err
	}
	opts, err := ortenv.SessionOptions(tune)
	if err != nil {
		return nil, err
	}
	defer func() { _ = opts.Destroy() }()

	g := &ortGraphs{}
	g.enrollSess, err = ort.NewDynamicAdvancedSession(f.Enroll,
		[]string{"enroll"}, []string{"enroll_mag", "context"}, opts)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot load %s", f.Enroll).WithCause(err)
	}
	g.chunkSess, err = ort.NewDynamicAdvancedSession(f.Chunk,
		[]string{"wav", "enroll_mag", "context", "h", "c"}, []string{"frames", "h_out", "c_out"}, opts)
	if err != nil {
		_ = g.enrollSess.Destroy()
		return nil, core.Errorf(core.CodeInternal, "cannot load %s", f.Chunk).WithCause(err)
	}
	return g, nil
}

func (g *ortGraphs) enroll(sample []float32) (*Voice, error) {
	in, err := ort.NewTensor(ort.NewShape(1, int64(len(sample))), sample)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Destroy() }()
	// The output sizes follow from the input length inside the graph; let
	// onnxruntime allocate them and copy out what it made.
	outs := []ort.Value{nil, nil}
	if err := g.enrollSess.Run([]ort.Value{in}, outs); err != nil {
		return nil, core.Errorf(core.CodeInternal, "voxlock enrollment graph failed").WithCause(err)
	}
	defer func() {
		for _, o := range outs {
			if o != nil {
				_ = o.Destroy()
			}
		}
	}()
	mag, magDim, err := copyOut(outs[0])
	if err != nil {
		return nil, err
	}
	ctx, ctxDim, err := copyOut(outs[1])
	if err != nil {
		return nil, err
	}
	return &Voice{mag: mag, ctx: ctx, magDim: magDim, ctxDim: ctxDim}, nil
}

func copyOut(v ort.Value) ([]float32, [3]int64, error) {
	t, ok := v.(*ort.Tensor[float32])
	if !ok {
		return nil, [3]int64{}, core.Errorf(core.CodeInternal, "voxlock: enrollment output is not float32")
	}
	shape := t.GetShape()
	if len(shape) != 3 {
		return nil, [3]int64{}, core.Errorf(core.CodeInternal, "voxlock: enrollment output has shape %v", shape)
	}
	return append([]float32(nil), t.GetData()...), [3]int64{shape[0], shape[1], shape[2]}, nil
}

func (g *ortGraphs) chunk(wav []float32, k int, v *Voice, h, c []float32) ([]float32, error) {
	if len(wav) != (k-1)*hop+nfft {
		return nil, fmt.Errorf("voxlock: %d samples is not %d frames", len(wav), k)
	}
	var values []ort.Value
	defer func() {
		for _, x := range values {
			_ = x.Destroy()
		}
	}()
	tensor := func(shape ort.Shape, data []float32) (*ort.Tensor[float32], error) {
		t, err := ort.NewTensor(shape, data)
		if err == nil {
			values = append(values, t)
		}
		return t, err
	}
	state := ort.NewShape(lstmLayers, lstmBatch, lstmHidden)
	frames := make([]float32, k*nfft)
	hOut, cOut := make([]float32, stateSize), make([]float32, stateSize)

	ins := make([]ort.Value, 5)
	outs := make([]ort.Value, 3)
	var err error
	if ins[0], err = tensor(ort.NewShape(1, int64(len(wav))), wav); err != nil {
		return nil, err
	}
	if ins[1], err = tensor(ort.NewShape(v.magDim[:]...), v.mag); err != nil {
		return nil, err
	}
	if ins[2], err = tensor(ort.NewShape(v.ctxDim[:]...), v.ctx); err != nil {
		return nil, err
	}
	if ins[3], err = tensor(state, h); err != nil {
		return nil, err
	}
	if ins[4], err = tensor(state, c); err != nil {
		return nil, err
	}
	if outs[0], err = tensor(ort.NewShape(1, int64(k), nfft), frames); err != nil {
		return nil, err
	}
	if outs[1], err = tensor(state, hOut); err != nil {
		return nil, err
	}
	if outs[2], err = tensor(state, cOut); err != nil {
		return nil, err
	}
	if err := g.chunkSess.Run(ins, outs); err != nil {
		return nil, core.Errorf(core.CodeInternal, "voxlock extraction graph failed").WithCause(err)
	}
	copy(h, hOut)
	copy(c, cOut)
	return frames, nil
}

func (g *ortGraphs) close() error {
	var first error
	for _, s := range []*ort.DynamicAdvancedSession{g.enrollSess, g.chunkSess} {
		if s == nil {
			continue
		}
		if err := s.Destroy(); err != nil && first == nil {
			first = err
		}
	}
	g.enrollSess, g.chunkSess = nil, nil
	return first
}
