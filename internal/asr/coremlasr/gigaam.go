// Package coremlasr runs NanoASR's recognisers as CoreML models, on the Neural
// Engine of an Apple Silicon Mac.
//
// The models are not onnxruntime graphs run through CoreML: they are the
// original networks, converted for CoreML with fixed shapes and fp16 weights,
// with every layer the Neural Engine lacks rewritten — LSTMs as matrix
// products, argmax as a comparison — so that nothing falls back to the CPU.
// scripts/coreml converts them; this package only drives them.
package coremlasr

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// Families this package loads.
const (
	FamilyGigaAMRNNT = "coreml_gigaam_rnnt"
	FamilyGigaAMCTC  = "coreml_gigaam_ctc"
)

// IsFamily reports whether a manifest family is one of this package's
// offline recognisers.
func IsFamily(family string) bool {
	return family == FamilyGigaAMRNNT || family == FamilyGigaAMCTC
}

// FamilyCapabilities is what a model of one of this package's families
// produces, for describing one that is not loaded. Punctuation is a property
// of the weights and is not answered here.
func FamilyCapabilities(family string) (core.Capabilities, bool) {
	switch family {
	case FamilyGigaAMRNNT, FamilyGigaAMCTC:
		return core.Capabilities{WordTimestamps: true, Confidence: true}, true
	case FamilyTOne, FamilyNemotron:
		return core.Capabilities{WordTimestamps: true}, true
	}
	return core.Capabilities{}, false
}

// gigaConfig is config.json, written by the converter beside the models.
type gigaConfig struct {
	Head        string  `json:"head"` // rnnt or ctc
	SampleRate  int     `json:"sample_rate"`
	Frames      int     `json:"window_frames"`
	Gap         int     `json:"gap_frames"`
	Hop         int     `json:"hop"`
	FrameLen    int     `json:"frame_length"`
	Scale       float32 `json:"audio_scale"`
	FrameShift  float64 `json:"frame_shift_seconds"`
	Blank       int     `json:"blank_id"`
	MaxSymbols  int     `json:"max_symbols_per_frame"`
	RopeDim     int     `json:"rope_dim"`
	EncoderOut  string  `json:"encoder_output"`
	EncoderDims int     `json:"encoder_dims"`
}

// Recognizer is GigaAM on the Neural Engine: one call of the encoder per
// window of packed segments, then a greedy RNN-T decoder that advances eight
// segments per call, or a CTC collapse that needs no call at all.
type Recognizer struct {
	cfg    gigaConfig
	lay    layout
	enc    *coreml.Model
	step   *coreml.Model // RNN-T only
	tokens []string
	caps   core.Capabilities
	unit   string

	// The step model's shapes: lanes decoded per call, encoder frames scanned
	// per call, joiner width, LSTM width, vocabulary.
	lanes, scan, joint, hidden, vocab int
}

// Load builds a recogniser from a CoreML GigaAM model directory.
func Load(_ context.Context, m registry.Manifest, dir string, v asr.Variant) (asr.Recognizer, error) {
	if v.Hotwords != "" {
		return nil, core.Errorf(core.CodeCapabilityUnavailable,
			"hotword biasing is not available on the CoreML build of %s: it decodes greedily", m.ID)
	}
	if err := asr.DecodingSupport(m.Family, v.DecodingMethod); err != nil {
		return nil, err
	}
	path := func(role string) (string, error) { return m.FilePath(dir, role) }
	cfgPath, err := path("config")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, core.Errorf(core.CodeModelNotFound, "model %s: %s", m.ID, err.Error()).WithCause(err)
	}
	r := &Recognizer{unit: m.ModelingUnit}
	if err := json.Unmarshal(b, &r.cfg); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: config.json", m.ID).WithCause(err)
	}
	if want := map[string]string{FamilyGigaAMRNNT: "rnnt", FamilyGigaAMCTC: "ctc"}[m.Family]; r.cfg.Head != want {
		return nil, core.Errorf(core.CodeInternal, "model %s: config.json describes a %s head, family %s wants %s",
			m.ID, r.cfg.Head, m.Family, want)
	}

	tokensPath, err := path("tokens")
	if err != nil {
		return nil, err
	}
	if r.tokens, err = readTokens(tokensPath); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: tokens", m.ID).WithCause(err)
	}
	ropePath, err := path("rope")
	if err != nil {
		return nil, err
	}
	t2 := subsample(subsample(r.cfg.Frames))
	rope, err := readFloats(ropePath, 2*t2*r.cfg.RopeDim)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: rope", m.ID).WithCause(err)
	}
	r.lay = layout{
		frames: r.cfg.Frames, gap: r.cfg.Gap, hop: r.cfg.Hop, frameLen: r.cfg.FrameLen,
		scale: r.cfg.Scale, ropeDim: r.cfg.RopeDim,
		cos: rope[:t2*r.cfg.RopeDim], sin: rope[t2*r.cfg.RopeDim:],
	}

	encPath, err := path("encoder")
	if err != nil {
		return nil, err
	}
	if r.enc, err = coreml.Load(encPath, "", coreml.NeuralEngine); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error()).WithCause(err)
	}
	if r.cfg.Head == "rnnt" {
		stepPath, err := path("step")
		if err != nil {
			r.Close()
			return nil, err
		}
		if r.step, err = coreml.Load(stepPath, "", coreml.NeuralEngine); err != nil {
			r.Close()
			return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error()).WithCause(err)
		}
		if err := r.readStepShapes(); err != nil {
			r.Close()
			return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error())
		}
	}

	r.caps = core.Capabilities{
		WordTimestamps:     true,
		Confidence:         true,
		PunctuationBuiltin: asr.VocabularyPunctuates(tokensPath),
		HotwordsReason:     "the CoreML build decodes greedily, and hotword biasing applies during beam search",
	}
	// The first prediction of a model is several times slower than the rest;
	// pay it now rather than in somebody's request.
	if _, err := r.Decode(context.Background(), [][]float32{make([]float32, r.cfg.SampleRate)}, r.cfg.SampleRate,
		asr.DecodeOptions{}); err != nil {
		r.Close()
		return nil, core.Errorf(core.CodeInternal, "model %s failed its warm-up decode", m.ID).WithCause(err)
	}
	return r, nil
}

func (r *Recognizer) readStepShapes() error {
	d := r.step.Description()
	onehot, err := d.Input("onehot")
	if err != nil {
		return err
	}
	enc, err := d.Input("enc")
	if err != nil {
		return err
	}
	h, err := d.Input("h")
	if err != nil {
		return err
	}
	if len(onehot.Shape) != 2 || len(enc.Shape) != 3 || len(h.Shape) != 2 {
		return fmt.Errorf("the step model's inputs are not shaped as expected")
	}
	r.lanes, r.vocab = onehot.Shape[0], onehot.Shape[1]
	r.scan, r.joint = enc.Shape[1], enc.Shape[2]
	r.hidden = h.Shape[1]
	if r.vocab != len(r.tokens) || r.joint != r.cfg.EncoderDims {
		return fmt.Errorf("the step model takes %d tokens and %d-wide frames, the encoder gives %d and %d",
			r.vocab, r.joint, len(r.tokens), r.cfg.EncoderDims)
	}
	return nil
}

// Decode recognises a batch of segments.
func (r *Recognizer) Decode(ctx context.Context, batch [][]float32, sampleRate int, _ asr.DecodeOptions) ([]asr.Recognition, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	if r.enc == nil {
		return nil, core.Errorf(core.CodeModelUnavailable, "recogniser is closed")
	}
	if sampleRate != r.cfg.SampleRate {
		resampled := make([][]float32, len(batch))
		for i, s := range batch {
			resampled[i] = audio.Resample(s, sampleRate, r.cfg.SampleRate)
		}
		batch = resampled
	}

	pieces := r.lay.split(batch)
	frames := make([][]float32, len(pieces)) // per piece: [encoder frames][dims]
	for _, w := range r.lay.pack(pieces) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := r.encode(w)
		if err != nil {
			return nil, err
		}
		for _, p := range w.places {
			frames[p.piece] = out[p.first*r.cfg.EncoderDims : (p.first+p.frames)*r.cfg.EncoderDims]
		}
	}

	var hyps []hypothesis
	var err error
	if r.cfg.Head == "rnnt" {
		hyps, err = r.greedyRNNT(ctx, frames)
	} else {
		hyps = r.greedyCTC(frames)
	}
	if err != nil {
		return nil, err
	}

	out := make([]asr.Recognition, len(batch))
	for i, p := range pieces {
		h := hyps[i]
		rec := &out[p.segment]
		offset := float32(float64(p.offset) / float64(r.cfg.SampleRate))
		for k, t := range h.tokens {
			rec.Tokens = append(rec.Tokens, r.tokens[t])
			rec.Timestamps = append(rec.Timestamps, offset+float32(float64(h.frames[k])*r.cfg.FrameShift))
			rec.LogProbs = append(rec.LogProbs, h.logProbs[k])
		}
	}
	for i := range out {
		out[i].Text = text(out[i].Tokens)
	}
	return out, nil
}

// encode runs the encoder over one window and returns [encoder frames][dims].
func (r *Recognizer) encode(w *window) ([]float32, error) {
	t2 := len(w.m2)
	out := make([]float32, t2*r.cfg.EncoderDims)
	err := r.enc.Predict(map[string][]float32{
		"audio": w.audio, "m0": w.m0, "m1": w.m1, "m2": w.m2,
		"amask": w.amask, "cos": w.cos, "sin": w.sin,
	}, map[string][]float32{r.cfg.EncoderOut: out})
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "encoder: %s", err.Error()).WithCause(err)
	}
	return out, nil
}

// hypothesis is one piece's decoded tokens, the encoder frame each was
// emitted at, and its log-probability.
type hypothesis struct {
	tokens   []int
	frames   []int
	logProbs []float32
}

// greedyRNNT decodes every piece greedily, r.lanes pieces per step call.
//
// Greedy RNN-T keeps the prediction network's state until a token is emitted,
// so the next emission is simply the first frame, from the current one on,
// whose best token is not blank. One step call advances the network by the
// last token and scores the next r.scan frames of every lane against it; the
// host finds the first non-blank frame. A lane with nothing in its window
// sends the same token and state again with the following frames: the step
// is a pure function, so that rescans without moving the network.
func (r *Recognizer) greedyRNNT(ctx context.Context, frames [][]float32) ([]hypothesis, error) {
	out := make([]hypothesis, len(frames))
	order := make([]int, 0, len(frames))
	for i, f := range frames {
		if len(f) > 0 {
			order = append(order, i)
		}
	}
	// Lanes of similar length finish together.
	slices.SortStableFunc(order, func(a, b int) int { return len(frames[b]) - len(frames[a]) })

	B, V, H, W, J := r.lanes, r.vocab, r.hidden, r.scan, r.joint
	onehot := make([]float32, B*V)
	h := make([]float32, B*H)
	c := make([]float32, B*H)
	enc := make([]float32, B*W*J)
	hOut := make([]float32, B*H)
	cOut := make([]float32, B*H)
	best := make([]float32, B*W)
	bestLP := make([]float32, B*W)

	for first := 0; first < len(order); first += B {
		lanes := order[first:min(first+B, len(order))]
		n := len(lanes)
		t := make([]int, n)
		sym := make([]int, n)
		done := make([]bool, B)
		for i := n; i < B; i++ {
			done[i] = true
		}
		clear(onehot)
		clear(h)
		clear(c)
		for i := range B {
			onehot[i*V+r.cfg.Blank] = 1
		}
		for !allDone(done) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			clear(enc)
			for i := range n {
				if done[i] {
					continue
				}
				f := frames[lanes[i]]
				lo := t[i] * J
				hi := min(len(f), lo+W*J)
				copy(enc[i*W*J:], f[lo:hi])
			}
			err := r.step.Predict(
				map[string][]float32{"onehot": onehot, "h": h, "c": c, "enc": enc},
				map[string][]float32{"h_out": hOut, "c_out": cOut, "best": best, "best_lp": bestLP})
			if err != nil {
				return nil, core.Errorf(core.CodeInternal, "decoder: %s", err.Error()).WithCause(err)
			}
			for i := range n {
				if done[i] {
					continue
				}
				total := len(frames[lanes[i]]) / J
				valid := min(W, total-t[i])
				k := -1
				for j := 0; j < valid; j++ {
					if int(best[i*W+j]) != r.cfg.Blank {
						k = j
						break
					}
				}
				if k < 0 {
					t[i] += W
					sym[i] = 0
					done[i] = t[i] >= total
					continue
				}
				if k > 0 {
					sym[i] = 0
				}
				t[i] += k
				tok := int(best[i*W+k])
				hy := &out[lanes[i]]
				hy.tokens = append(hy.tokens, tok)
				hy.frames = append(hy.frames, t[i])
				hy.logProbs = append(hy.logProbs, bestLP[i*W+k])
				sym[i]++
				// The next call advances this lane by the token just emitted.
				copy(h[i*H:(i+1)*H], hOut[i*H:(i+1)*H])
				copy(c[i*H:(i+1)*H], cOut[i*H:(i+1)*H])
				clear(onehot[i*V : (i+1)*V])
				onehot[i*V+tok] = 1
				if sym[i] >= r.cfg.MaxSymbols {
					t[i]++
					sym[i] = 0
					done[i] = t[i] >= total
				}
			}
		}
	}
	return out, nil
}

func allDone(done []bool) bool {
	for _, d := range done {
		if !d {
			return false
		}
	}
	return true
}

// greedyCTC collapses the best token of each frame: repeats merge, blanks
// separate. The encoder already returns log-probabilities.
func (r *Recognizer) greedyCTC(frames [][]float32) []hypothesis {
	out := make([]hypothesis, len(frames))
	V := r.cfg.EncoderDims
	for i, f := range frames {
		prev := -1
		for t := 0; t < len(f)/V; t++ {
			row := f[t*V : (t+1)*V]
			k := 0
			for j := 1; j < V; j++ {
				if row[j] > row[k] {
					k = j
				}
			}
			if k != r.cfg.Blank && k != prev {
				out[i].tokens = append(out[i].tokens, k)
				out[i].frames = append(out[i].frames, t)
				out[i].logProbs = append(out[i].logProbs, row[k])
			}
			prev = k
		}
	}
	return out
}

// text joins subword tokens the way sherpa-onnx does for these models: the
// word-boundary mark becomes a space.
func text(tokens []string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.Join(tokens, ""), "▁", " "))
}

func (r *Recognizer) Capabilities() core.Capabilities { return r.caps }
func (r *Recognizer) ModelingUnit() string            { return r.unit }

// Close frees the models. It is safe to call more than once.
func (r *Recognizer) Close() error {
	if r.enc != nil {
		r.enc.Close()
		r.enc = nil
	}
	if r.step != nil {
		r.step.Close()
		r.step = nil
	}
	return nil
}

var _ asr.Recognizer = (*Recognizer)(nil)

// readTokens reads a sherpa-onnx tokens.txt: "<symbol> <id>" per line.
func readTokens(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		id, err := strconv.Atoi(line[i+1:])
		if err != nil {
			return nil, fmt.Errorf("%s: %q", path, line)
		}
		for len(out) <= id {
			out = append(out, "")
		}
		out[id] = line[:i]
	}
	return out, sc.Err()
}

func readFloats(path string, n int) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != 4*n {
		return nil, fmt.Errorf("%s is %d bytes, want %d float32", path, len(b), n)
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out, nil
}
