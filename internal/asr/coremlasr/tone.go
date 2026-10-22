package coremlasr

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// FamilyTOne is T-one converted for CoreML.
const FamilyTOne = "coreml_t_one_ctc"

// T-one decodes 300 ms of 8 kHz audio per call into ten 30 ms frames.
const (
	toneRate       = 8000
	toneChunk      = 2400
	toneFrameShift = 0.03
)

// toneCacheLens are the values the attention cache length takes, one per
// function of the model: 0, 10 and 20 for the first three chunks of an
// utterance, 30 from then on. The model's only integer state, baked in so the
// graph keeps none.
var toneCacheLens = []int{0, 10, 20, 30}

// toneStates are the six state tensors a chunk takes and returns.
var toneStates = []string{"s_pre", "s_mhsa", "s_conv", "s_sub1", "s_sub2", "s_red"}

// TOne is T-one on CoreML, run on the GPU.
//
// The GPU and not the Neural Engine, as measured: every op of the converted
// graph is one the Neural Engine supports, and CoreML still plans all of it
// for the CPU there, while the GPU runs a chunk in 9 ms — 33 times real time
// for one stream. Its transcripts match the ONNX model's to 0.05% CER.
type TOne struct {
	fns     []*coreml.Model // by index into toneCacheLens
	tokens  []string
	blank   int
	modelID string
	unit    string
	caps    core.Capabilities
	end     endpoints
	sizes   map[string]int
	vocab   int
}

// LoadTOne loads the CoreML T-one from its model directory.
func LoadTOne(ctx context.Context, m registry.Manifest, dir string, opt StreamingOptions) (*TOne, error) {
	modelPath, err := m.FilePath(dir, "model")
	if err != nil {
		return nil, err
	}
	tokensPath, err := m.FilePath(dir, "tokens")
	if err != nil {
		return nil, err
	}
	t := &TOne{modelID: m.Key(), unit: m.ModelingUnit, end: newEndpoints(opt), sizes: map[string]int{}}
	if t.tokens, err = readTokens(tokensPath); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: tokens", m.ID).WithCause(err)
	}
	t.blank = -1
	for i, s := range t.tokens {
		if s == "<blk>" || s == "<blank>" {
			t.blank = i
		}
	}
	if t.blank < 0 {
		return nil, core.Errorf(core.CodeInternal, "model %s: no blank token in %s", m.ID, tokensPath)
	}
	for _, l := range toneCacheLens {
		f, err := coreml.Load(modelPath, "len"+strconv.Itoa(l), coreml.CPUAndGPU)
		if err != nil {
			t.Close()
			return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error()).WithCause(err)
		}
		t.fns = append(t.fns, f)
	}
	d := t.fns[0].Description()
	for _, name := range toneStates {
		in, err := d.Input(name)
		if err != nil {
			t.Close()
			return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error())
		}
		t.sizes[name] = in.Size()
	}
	lp, err := d.Output("logprobs")
	if err != nil || len(lp.Shape) != 3 || lp.Shape[2] != len(t.tokens) {
		t.Close()
		return nil, core.Errorf(core.CodeInternal, "model %s: the logprobs do not match the %d tokens", m.ID, len(t.tokens))
	}
	t.vocab = lp.Shape[2]
	t.caps = core.Capabilities{WordTimestamps: true, PunctuationBuiltin: asr.VocabularyPunctuates(tokensPath)}

	// The first prediction pays for compiling the graph for the GPU; a
	// realtime caller must not.
	s, _ := t.NewStream()
	s.Accept(toneRate, make([]float32, toneRate))
	s.Finish()
	for s.Ready() {
		if err := ctx.Err(); err != nil {
			t.Close()
			return nil, err
		}
		t.Decode([]asr.Stream{s})
	}
	return t, nil
}

func (t *TOne) NewStream() (asr.Stream, error) {
	if len(t.fns) == 0 {
		return nil, core.Errorf(core.CodeModelUnavailable, "streaming recogniser is closed")
	}
	s := &toneStream{t: t}
	s.resetState()
	return s, nil
}

// Decode advances every ready stream by one chunk. The model is batch 1, so
// a batch is decoded one stream after another.
func (t *TOne) Decode(streams []asr.Stream) {
	for _, st := range streams {
		s, ok := st.(*toneStream)
		if !ok || s.t != t || !s.Ready() {
			continue
		}
		s.decode()
	}
}

func (t *TOne) SampleRate() int                 { return toneRate }
func (t *TOne) ModelID() string                 { return t.modelID }
func (t *TOne) ModelingUnit() string            { return t.unit }
func (t *TOne) Capabilities() core.Capabilities { return t.caps }

func (t *TOne) Close() error {
	for _, f := range t.fns {
		f.Close()
	}
	t.fns = nil
	return nil
}

var _ asr.Streaming = (*TOne)(nil)

// toneStream is one connection's decoder state, mirroring what sherpa-onnx's
// OnlineStream and greedy CTC decoder keep for this model.
type toneStream struct {
	t        *TOne
	rs       *streamResampler
	rsRate   int
	pending  []float32 // 8 kHz samples not yet decoded
	finished bool

	state  map[string][]float32
	chunks int // chunks decoded since the utterance began

	tokens         []int
	frames         []int // the frame each token was emitted at
	trailingBlanks int
	frameOffset    int
	lastID         int
	err            error
}

func (s *toneStream) resetState() {
	s.state = map[string][]float32{}
	for name, n := range s.t.sizes {
		s.state[name] = make([]float32, n)
	}
	s.chunks, s.tokens, s.frames, s.trailingBlanks, s.frameOffset = 0, nil, nil, 0, 0
}

func (s *toneStream) Accept(sampleRate int, samples []float32) {
	if s.finished || len(samples) == 0 {
		return
	}
	if sampleRate == toneRate {
		s.pending = append(s.pending, samples...)
		return
	}
	if s.rs == nil || s.rsRate != sampleRate {
		if s.rs != nil {
			s.pending = append(s.pending, s.rs.flush()...)
		}
		s.rs, s.rsRate = newStreamResampler(sampleRate, toneRate), sampleRate
	}
	s.pending = append(s.pending, s.rs.push(samples)...)
}

func (s *toneStream) SetLanguage(string) {}

func (s *toneStream) Ready() bool { return len(s.pending) >= toneChunk && s.err == nil }

func (s *toneStream) decode() {
	// sherpa-onnx's input: the float clamped to [-1, 1] and truncated to
	// int16 steps, which the graph scales back.
	signal := make([]float32, toneChunk)
	for i, v := range s.pending[:toneChunk] {
		v = max(-1, min(1, v))
		signal[i] = float32(math.Trunc(float64(v)*32767)) / 32767
	}
	s.pending = s.pending[toneChunk:]

	in := map[string][]float32{"signal": signal}
	out := map[string][]float32{"logprobs": make([]float32, 10*s.t.vocab)}
	next := map[string][]float32{}
	for _, name := range toneStates {
		in[name] = s.state[name]
		next[name] = make([]float32, s.t.sizes[name])
		out[name+"_out"] = next[name]
	}
	f := s.t.fns[min(s.chunks, len(toneCacheLens)-1)]
	if err := f.Predict(in, out); err != nil {
		s.err = err
		return
	}
	s.state = next
	s.chunks++

	// sherpa-onnx's OnlineCtcGreedySearchDecoder, carried across chunks.
	prev := -1
	if len(s.tokens) > 0 {
		prev = s.tokens[len(s.tokens)-1]
		if s.trailingBlanks > 0 {
			prev = s.t.blank
		}
	}
	lp := out["logprobs"]
	frames := len(lp) / s.t.vocab
	for f := 0; f < frames; f++ {
		row := lp[f*s.t.vocab : (f+1)*s.t.vocab]
		y := 0
		for k := 1; k < len(row); k++ {
			if row[k] > row[y] {
				y = k
			}
		}
		if y == s.t.blank {
			s.trailingBlanks++
		} else {
			s.trailingBlanks = 0
		}
		if y != s.t.blank && y != prev {
			s.tokens = append(s.tokens, y)
			s.frames = append(s.frames, f+s.frameOffset)
		}
		prev = y
	}
	s.frameOffset += frames
}

func (s *toneStream) Hypothesis() asr.Hypothesis {
	h := asr.Hypothesis{}
	var b strings.Builder
	for i, id := range s.tokens {
		sym := s.t.tokens[id]
		b.WriteString(sym)
		h.Tokens = append(h.Tokens, sym)
		h.Timestamps = append(h.Timestamps, float32(float64(s.frames[i])*toneFrameShift))
	}
	h.Text = b.String()
	return h
}

// Endpoint is sherpa-onnx's rule for this model, including its arithmetic:
// the utterance length it compares is the number of 300 ms chunks decoded
// times the 30 ms frame shift. That undercounts the utterance tenfold, which
// in effect leaves the rule of trailing silence alone deciding; it is kept so
// that switching to CoreML moves no turn boundary.
func (s *toneStream) Endpoint() bool {
	return s.t.end.is(float32(float64(s.chunks)*toneFrameShift), float32(float64(s.trailingBlanks)*toneFrameShift))
}

func (s *toneStream) Reset() { s.resetState() }

// Finish flushes the resampler and pads the tail to a whole chunk, so the
// last fraction of a chunk is decoded rather than dropped.
func (s *toneStream) Finish() {
	if s.finished {
		return
	}
	if s.rs != nil {
		s.pending = append(s.pending, s.rs.flush()...)
	}
	if r := len(s.pending) % toneChunk; r != 0 {
		s.pending = append(s.pending, make([]float32, toneChunk-r)...)
	}
	s.finished = true
}

func (s *toneStream) Close() {}

var _ asr.Stream = (*toneStream)(nil)

// String names the model for logs.
func (t *TOne) String() string { return fmt.Sprintf("t-one on CoreML (%s)", t.modelID) }

// IsStreamingFamily reports whether a family is one of this package's
// streaming recognisers.
func IsStreamingFamily(family string) bool {
	return family == FamilyTOne || family == FamilyNemotron
}

// LoadStreaming loads a streaming recogniser of this package by family.
func LoadStreaming(ctx context.Context, m registry.Manifest, dir string, opt StreamingOptions) (asr.Streaming, error) {
	switch m.Family {
	case FamilyTOne:
		return LoadTOne(ctx, m, dir, opt)
	case FamilyNemotron:
		return LoadNemotron(ctx, m, dir, opt)
	}
	return nil, core.Errorf(core.CodeInvalidRequest, "model %s: family %s is not a CoreML streaming family", m.ID, m.Family)
}
