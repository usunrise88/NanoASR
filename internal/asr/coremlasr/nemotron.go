package coremlasr

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// FamilyNemotron is NVIDIA's Nemotron 3.5 ASR streaming (cache-aware
// FastConformer, RNN-T, language as a prompt) in FluidInference's CoreML
// build — the models FluidAudio drives, which this file drives the same way
// (StreamingNemotronMultilingualAsrManager, FluidAudio main at c79c4106).
const FamilyNemotron = "coreml_nemotron_streaming"

// The front end and the encoder's geometry at 16 kHz.
const (
	nemoRate       = 16000
	nemoHop        = 160
	nemoWin        = 400
	nemoFFT        = 512
	nemoMels       = 128
	nemoPreCache   = 9    // mel frames of the previous chunk the encoder sees first
	nemoFrameShift = 0.08 // seconds per encoder frame (8 mel frames)
	nemoMaxSymbols = 10   // tokens one encoder frame may emit
)

// nemoMeta is metadata.json, as the CoreML build ships it.
type nemoMeta struct {
	ChunkMelFrames   int            `json:"chunk_mel_frames"`
	TotalMelFrames   int            `json:"total_mel_frames"`
	PreEncodeCache   int            `json:"pre_encode_cache"`
	VocabSize        int            `json:"vocab_size"`
	BlankIdx         int            `json:"blank_idx"`
	CacheChannel     []int          `json:"cache_channel_shape"`
	CacheTime        []int          `json:"cache_time_shape"`
	DecoderHidden    int            `json:"decoder_hidden"`
	DecoderLayers    int            `json:"decoder_layers"`
	EncoderDim       int            `json:"encoder_dim"`
	DefaultPromptID  int            `json:"default_prompt_id"`
	PromptDictionary map[string]int `json:"prompt_dictionary"`
	LangTagTokenIDs  []int          `json:"lang_tag_token_ids"`
}

// Nemotron is the streaming recogniser: the encoder and the fused
// decoder-joint step on the Neural Engine, the log-mel front end on the host.
type Nemotron struct {
	enc, step *coreml.Model
	meta      nemoMeta
	pieces    map[int]string
	langTag   map[int]bool
	modelID   string
	unit      string
	caps      core.Capabilities
	end       endpoints

	mel     [][]float64 // [nemoMels][nemoFFT/2+1] Slaney filterbank
	window  []float64   // symmetric Hann, nemoWin long
	fft     *realFFT
	encT    int // encoder frames per chunk
	chunk   int // samples per chunk
	ccSize  int
	ctSize  int
	hidSize int
}

// LoadNemotron loads the CoreML build from its model directory.
func LoadNemotron(ctx context.Context, m registry.Manifest, dir string, opt StreamingOptions) (*Nemotron, error) {
	path := func(role string) (string, error) { return m.FilePath(dir, role) }
	n := &Nemotron{modelID: m.Key(), unit: m.ModelingUnit, end: newEndpoints(opt)}
	metaPath, err := path("metadata")
	if err != nil {
		return nil, err
	}
	if err := readJSON(metaPath, &n.meta); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: metadata", m.ID).WithCause(err)
	}
	tokPath, err := path("tokens")
	if err != nil {
		return nil, err
	}
	var raw map[string]string
	if err := readJSON(tokPath, &raw); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: tokenizer", m.ID).WithCause(err)
	}
	n.pieces = map[int]string{}
	for k, v := range raw {
		if id, err := strconv.Atoi(k); err == nil {
			n.pieces[id] = v
		}
	}
	n.langTag = map[int]bool{}
	for _, id := range n.meta.LangTagTokenIDs {
		n.langTag[id] = true
	}
	encPath, err := path("encoder")
	if err != nil {
		return nil, err
	}
	stepPath, err := path("decoder_joint")
	if err != nil {
		return nil, err
	}
	// The Neural Engine and not "all": with every unit allowed, CoreML puts
	// this int8 encoder on the GPU, where it runs ten times slower.
	if n.enc, err = coreml.Load(encPath, "", coreml.NeuralEngine); err != nil {
		return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error()).WithCause(err)
	}
	if n.step, err = coreml.Load(stepPath, "", coreml.NeuralEngine); err != nil {
		n.Close()
		return nil, core.Errorf(core.CodeInternal, "model %s: %s", m.ID, err.Error()).WithCause(err)
	}
	encoded, err := n.enc.Description().Output("encoded")
	if err != nil || len(encoded.Shape) != 3 {
		n.Close()
		return nil, core.Errorf(core.CodeInternal, "model %s: the encoder has no encoded output", m.ID)
	}
	n.encT = encoded.Shape[2]
	n.chunk = n.meta.ChunkMelFrames * nemoHop
	n.ccSize, n.ctSize = prod(n.meta.CacheChannel), prod(n.meta.CacheTime)
	n.hidSize = n.meta.DecoderLayers * n.meta.DecoderHidden
	n.mel = slaneyMel(nemoMels, nemoFFT, nemoRate, 0, nemoRate/2)
	n.window = make([]float64, nemoWin)
	for i := range n.window {
		n.window[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(nemoWin-1)))
	}
	n.fft = newRealFFT(nemoFFT)
	n.caps = core.Capabilities{WordTimestamps: true, PunctuationBuiltin: true, LanguageDetect: true}

	// FluidAudio warns that a Neural Engine program can come up stillborn and
	// return an all-zero encoding without an error; one decode of a tone at
	// load both warms the models and catches that.
	s, _ := n.NewStream()
	tone := make([]float32, n.chunk)
	for i := range tone {
		tone[i] = float32(0.1 * math.Sin(2*math.Pi*440*float64(i)/nemoRate))
	}
	s.Accept(nemoRate, tone)
	ns := s.(*nemoStream)
	if err := ns.decode(); err != nil {
		n.Close()
		return nil, core.Errorf(core.CodeInternal, "model %s failed its warm-up decode", m.ID).WithCause(err)
	}
	if ns.lastEncodedZero {
		n.Close()
		return nil, core.Errorf(core.CodeInternal,
			"model %s: the encoder returned all zeros at load, a Neural Engine program that did not start; restart to retry", m.ID)
	}
	return n, nil
}

func prod(s []int) int {
	p := 1
	for _, d := range s {
		p *= d
	}
	return p
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// promptID resolves a language the way FluidAudio does: the exact key, then
// with "_" as "-", then lower-case language and upper-case region, then the
// bare language; anything else, "auto" included, is the default prompt.
func (n *Nemotron) promptID(lang string) int {
	d := n.meta.PromptDictionary
	if id, ok := d[lang]; ok && lang != "" {
		return id
	}
	l := strings.ReplaceAll(lang, "_", "-")
	if id, ok := d[l]; ok {
		return id
	}
	if parts := strings.SplitN(l, "-", 2); len(parts) == 2 {
		if id, ok := d[strings.ToLower(parts[0])+"-"+strings.ToUpper(parts[1])]; ok {
			return id
		}
		l = parts[0]
	}
	if id, ok := d[strings.ToLower(l)]; ok {
		return id
	}
	return n.meta.DefaultPromptID
}

func (n *Nemotron) NewStream() (asr.Stream, error) {
	if n.enc == nil {
		return nil, core.Errorf(core.CodeModelUnavailable, "streaming recogniser is closed")
	}
	s := &nemoStream{n: n, prompt: n.meta.DefaultPromptID}
	s.resetState()
	return s, nil
}

// Decode advances every ready stream by one chunk; the models are batch 1.
func (n *Nemotron) Decode(streams []asr.Stream) {
	for _, st := range streams {
		s, ok := st.(*nemoStream)
		if !ok || s.n != n || !s.Ready() {
			continue
		}
		if err := s.decode(); err != nil {
			s.err = err
		}
	}
}

func (n *Nemotron) SampleRate() int                 { return nemoRate }
func (n *Nemotron) ModelID() string                 { return n.modelID }
func (n *Nemotron) ModelingUnit() string            { return n.unit }
func (n *Nemotron) Capabilities() core.Capabilities { return n.caps }

func (n *Nemotron) Close() error {
	if n.enc != nil {
		n.enc.Close()
		n.enc = nil
	}
	if n.step != nil {
		n.step.Close()
		n.step = nil
	}
	return nil
}

var _ asr.Streaming = (*Nemotron)(nil)

// logMel is FluidAudio's NemotronMelExtractor over one chunk: pre-emphasis
// from a fresh state, 256 zeros of centre padding on each side, a 400-sample
// symmetric Hann window at offset 56 of every 512-sample frame, the power
// spectrum, the Slaney filterbank and ln(x + 2^-24). It returns [mel][frame].
func (n *Nemotron) logMel(x []float32) [][]float32 {
	pad := nemoFFT / 2
	y := make([]float64, len(x)+2*pad)
	prev := 0.0
	for i, v := range x {
		y[pad+i] = float64(v) - 0.97*prev
		prev = float64(v)
	}
	frames := 1 + (len(y)-nemoWin)/nemoHop
	out := make([][]float32, nemoMels)
	for m := range out {
		out[m] = make([]float32, frames)
	}
	frame := make([]float64, nemoFFT)
	power := make([]float64, nemoFFT/2+1)
	re, im := make([]float64, nemoFFT/2), make([]float64, nemoFFT/2)
	off := (nemoFFT - nemoWin) / 2
	guard := math.Pow(2, -24)
	for t := 0; t < frames; t++ {
		clear(frame)
		start := t * nemoHop
		for i := 0; i < nemoWin; i++ {
			if j := start + off + i; j < len(y) {
				frame[off+i] = y[j] * n.window[i]
			}
		}
		n.fft.power(frame, power, re, im)
		for m, filt := range n.mel {
			sum := 0.0
			for k, w := range filt {
				if w != 0 {
					sum += w * power[k]
				}
			}
			out[m][t] = float32(math.Log(sum + guard))
		}
	}
	return out
}

// slaneyMel is librosa.filters.mel(htk=False, norm="slaney").
func slaneyMel(nMels, nFFT, rate int, fmin, fmax float64) [][]float64 {
	const fsp, minLogHz = 200.0 / 3, 1000.0
	minLogMel := minLogHz / fsp
	logStep := math.Log(6.4) / 27
	toMel := func(f float64) float64 {
		if f >= minLogHz {
			return minLogMel + math.Log(f/minLogHz)/logStep
		}
		return f / fsp
	}
	toHz := func(m float64) float64 {
		if m >= minLogMel {
			return minLogHz * math.Exp(logStep*(m-minLogMel))
		}
		return fsp * m
	}
	bins := nFFT/2 + 1
	lo, hi := toMel(fmin), toMel(fmax)
	pts := make([]float64, nMels+2)
	for i := range pts {
		pts[i] = toHz(lo + (hi-lo)*float64(i)/float64(nMels+1))
	}
	out := make([][]float64, nMels)
	for m := range out {
		out[m] = make([]float64, bins)
		enorm := 2 / (pts[m+2] - pts[m])
		for k := 0; k < bins; k++ {
			f := float64(k) * float64(rate) / float64(nFFT)
			lower := (f - pts[m]) / (pts[m+1] - pts[m])
			upper := (pts[m+2] - f) / (pts[m+2] - pts[m+1])
			if w := math.Min(lower, upper); w > 0 {
				out[m][k] = w * enorm
			}
		}
	}
	return out
}

// nemoStream is one connection's state, the per-stream half of FluidAudio's
// manager. Not ported: its "blank rescue", which re-decodes a short word the
// carried state swallowed, and its opt-in hotword bias.
type nemoStream struct {
	n        *Nemotron
	rs       *streamResampler
	rsRate   int
	pending  []float32
	finished bool
	prompt   int
	err      error

	cacheChannel, cacheTime []float32
	cacheLen                float32
	melCache                [][]float32 // [mel][nemoPreCache], nil before the first chunk
	h, c                    []float32
	lastToken               int

	ids            []int
	frames         []int
	frameBase      int
	trailingBlanks int // encoder frames since the last token
	decodedFrames  int

	lastEncodedZero bool
}

func (s *nemoStream) resetState() {
	s.cacheChannel = make([]float32, s.n.ccSize)
	s.cacheTime = make([]float32, s.n.ctSize)
	// 1, not 0: a zero length makes a zero-size slice CoreML refuses.
	s.cacheLen = 1
	s.melCache = nil
	s.h = make([]float32, s.n.hidSize)
	s.c = make([]float32, s.n.hidSize)
	s.lastToken = s.n.meta.BlankIdx
	s.ids, s.frames, s.frameBase, s.trailingBlanks, s.decodedFrames = nil, nil, 0, 0, 0
}

func (s *nemoStream) Accept(sampleRate int, samples []float32) {
	if s.finished || len(samples) == 0 {
		return
	}
	if sampleRate == nemoRate {
		s.pending = append(s.pending, samples...)
		return
	}
	if s.rs == nil || s.rsRate != sampleRate {
		if s.rs != nil {
			s.pending = append(s.pending, s.rs.flush()...)
		}
		s.rs, s.rsRate = newStreamResampler(sampleRate, nemoRate), sampleRate
	}
	s.pending = append(s.pending, s.rs.push(samples)...)
}

func (s *nemoStream) SetLanguage(lang string) { s.prompt = s.n.promptID(lang) }

func (s *nemoStream) Ready() bool { return len(s.pending) >= s.n.chunk && s.err == nil }

func (s *nemoStream) decode() error {
	n := s.n
	chunk := s.pending[:n.chunk]
	s.pending = s.pending[n.chunk:]

	mel := n.logMel(chunk) // [128][57]
	total := n.meta.TotalMelFrames
	in := make([]float32, nemoMels*total)
	pre := n.meta.PreEncodeCache
	for m := 0; m < nemoMels; m++ {
		row := in[m*total : (m+1)*total]
		if s.melCache != nil {
			copy(row[:pre], s.melCache[m])
		}
		copy(row[pre:], mel[m][:total-pre])
	}
	// The next chunk's context is this chunk's last frames, the one computed
	// from the right padding included, as FluidAudio keeps it.
	s.melCache = make([][]float32, nemoMels)
	frames := len(mel[0])
	for m := range s.melCache {
		s.melCache[m] = append([]float32(nil), mel[m][frames-pre:]...)
	}

	encoded := make([]float32, n.meta.EncoderDim*n.encT)
	cc, ct := make([]float32, n.ccSize), make([]float32, n.ctSize)
	cl := make([]float32, 1)
	err := n.enc.Predict(map[string][]float32{
		"mel": in, "mel_length": {float32(total)},
		"cache_channel": s.cacheChannel, "cache_time": s.cacheTime,
		"cache_len": {s.cacheLen}, "prompt_id": {float32(s.prompt)},
	}, map[string][]float32{
		"encoded": encoded, "cache_channel_out": cc, "cache_time_out": ct, "cache_len_out": cl,
	})
	if err != nil {
		return err
	}
	s.cacheChannel, s.cacheTime, s.cacheLen = cc, ct, cl[0]
	s.lastEncodedZero = allZero(encoded)

	// Greedy RNN-T over the chunk's frames with the fused decoder-joint step.
	D := n.meta.EncoderDim
	step := make([]float32, D)
	logits := make([]float32, n.meta.VocabSize+1)
	hOut, cOut := make([]float32, n.hidSize), make([]float32, n.hidSize)
	for t := 0; t < n.encT; t++ {
		for d := 0; d < D; d++ {
			step[d] = encoded[d*n.encT+t]
		}
		emitted := false
		for sym := 0; sym < nemoMaxSymbols; sym++ {
			err := n.step.Predict(map[string][]float32{
				"token": {float32(s.lastToken)}, "token_length": {1},
				"h_in": s.h, "c_in": s.c, "encoder": step,
			}, map[string][]float32{"logits": logits, "h_out": hOut, "c_out": cOut})
			if err != nil {
				return err
			}
			k := 0
			for j := 1; j < len(logits); j++ {
				if logits[j] > logits[k] {
					k = j
				}
			}
			if k == n.meta.BlankIdx {
				break
			}
			s.ids = append(s.ids, k)
			s.frames = append(s.frames, s.frameBase+t)
			s.lastToken = k
			copy(s.h, hOut)
			copy(s.c, cOut)
			if !n.langTag[k] {
				emitted = true
			}
		}
		if emitted {
			s.trailingBlanks = 0
		} else {
			s.trailingBlanks++
		}
	}
	s.frameBase += n.encT
	s.decodedFrames += n.encT
	return nil
}

func allZero(x []float32) bool {
	for _, v := range x {
		if v != 0 {
			return false
		}
	}
	return true
}

var spaces = regexp.MustCompile(` {2,}`)

// Hypothesis is the text of the utterance so far, language tags left out,
// as FluidAudio's tokenizer renders it.
func (s *nemoStream) Hypothesis() asr.Hypothesis {
	var h asr.Hypothesis
	var b strings.Builder
	for i, id := range s.ids {
		if s.n.langTag[id] {
			continue
		}
		p, ok := s.n.pieces[id]
		if !ok {
			continue
		}
		b.WriteString(p)
		h.Tokens = append(h.Tokens, p)
		h.Timestamps = append(h.Timestamps, float32(float64(s.frames[i])*nemoFrameShift))
	}
	text := strings.ReplaceAll(b.String(), "▁", " ")
	h.Text = spaces.ReplaceAllString(strings.Trim(text, " \t"), " ")
	return h
}

// Endpoint applies sherpa-onnx's rules, as its streaming transducers do:
// encoder frames decoded and trailing frames without a token, at 80 ms each.
func (s *nemoStream) Endpoint() bool {
	return s.n.end.is(float32(float64(s.decodedFrames)*nemoFrameShift),
		float32(float64(s.trailingBlanks)*nemoFrameShift))
}

func (s *nemoStream) Reset() { s.resetState() }

// Finish pads what is left to a whole chunk, as FluidAudio's finish() does.
func (s *nemoStream) Finish() {
	if s.finished {
		return
	}
	if s.rs != nil {
		s.pending = append(s.pending, s.rs.flush()...)
	}
	if r := len(s.pending) % s.n.chunk; r != 0 {
		s.pending = append(s.pending, make([]float32, s.n.chunk-r)...)
	}
	s.finished = true
}

func (s *nemoStream) Close() {}

var _ asr.Stream = (*nemoStream)(nil)
