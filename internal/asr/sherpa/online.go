package sherpa

import (
	"context"
	"os"
	"runtime"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// OnlineOptions configure a streaming recogniser. Unlike the offline loader
// these are not per-request: one recogniser serves every connection, and
// sherpa-onnx settles decoding method, endpointing and hotwords when it is
// constructed.
type OnlineOptions struct {
	Provider   string
	NumThreads int
	Debug      bool

	DecodingMethod string
	MaxActivePaths int

	// EnableEndpoint turns on utterance segmentation. With it off, an
	// utterance only ends when the caller says so.
	EnableEndpoint          bool
	Rule1MinTrailingSilence float32
	Rule2MinTrailingSilence float32
	Rule3MinUtteranceLength float32

	// SkipWarmup disables the warm-up decode. Only tests should set it.
	SkipWarmup bool
}

// Online is one loaded streaming recogniser.
//
// The weights are shared by every stream; what is per-connection is the small
// amount of state inside Stream. That split is why a streaming model can serve
// many callers from one copy in memory, and it is also why every method here
// has to be told which stream it is talking about.
type Online struct {
	mu     sync.RWMutex
	impl   *sonnx.OnlineRecognizer
	closed bool

	modelID      string
	sampleRate   int
	modelingUnit string
	caps         core.Capabilities

	// batch is reused across Decode calls. The decoder loop runs fifty times a
	// second; allocating a slice per call would be the only garbage this path
	// produces.
	batch []*sonnx.OnlineStream
}

// NewOnline loads a streaming model from its manifest.
func NewOnline(ctx context.Context, m registry.Manifest, dir string, opt OnlineOptions) (*Online, error) {
	if kind := m.EffectiveKind(); kind != registry.KindASR {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"model %s is a %s model and cannot transcribe", m.ID, kind)
	}
	if !m.Streaming {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"model %s is not a streaming model: realtime recognition needs one "+
				"(the catalog marks them `streaming: true`)", m.ID)
	}

	fam, err := LookupOnlineFamily(m.Family)
	if err != nil {
		return nil, err
	}
	if err := fam.Validate(m.Files); err != nil {
		return nil, err
	}
	tokens, err := m.FilePath(dir, "tokens")
	if err != nil {
		return nil, err
	}

	if opt.NumThreads < 1 {
		opt.NumThreads = 1
	}
	if opt.DecodingMethod == "" {
		opt.DecodingMethod = "greedy_search"
	}
	if opt.MaxActivePaths < 1 {
		opt.MaxActivePaths = 4
	}

	cfg := sonnx.OnlineRecognizerConfig{
		FeatConfig: sonnx.FeatureConfig{
			SampleRate: m.FeatureSampleRate(),
			FeatureDim: m.Features.Dim,
		},
		ModelConfig: sonnx.OnlineModelConfig{
			Tokens:     tokens,
			NumThreads: numThreads(m, LoaderOptions{NumThreads: opt.NumThreads}),
			Provider:   providerOrCPU(opt.Provider),
			ModelType:  m.Runtime.ModelType,
			Debug:      boolToInt(opt.Debug),
		},
		DecodingMethod: opt.DecodingMethod,
		MaxActivePaths: opt.MaxActivePaths,
		EnableEndpoint: boolToInt(opt.EnableEndpoint),
		// Passed whatever EnableEndpoint says: sherpa-onnx ignores them when
		// endpointing is off, and zeroing them here would make turning it back
		// on at runtime behave differently from a restart.
		Rule1MinTrailingSilence: opt.Rule1MinTrailingSilence,
		Rule2MinTrailingSilence: opt.Rule2MinTrailingSilence,
		Rule3MinUtteranceLength: opt.Rule3MinUtteranceLength,
		BlankPenalty:            m.Runtime.BlankPenalty,
	}

	// Hotwords are deliberately absent. They are fixed at construction here
	// too, so honouring a per-session bias list would mean a second resident
	// copy of the model for the life of a connection — and on the streaming
	// side sherpa-onnx terminates the process for an unsupported modeling unit
	// rather than returning an error. The realtime dialect reports the
	// omission to clients that send `prompt` instead.
	if err := asr.DecodingSupport(m.Family, cfg.DecodingMethod); err != nil {
		return nil, err
	}
	if err := fam.Configure(m, dir, &cfg.ModelConfig); err != nil {
		return nil, err
	}
	if err := checkOnlineFilesExist(m, cfg.ModelConfig, tokens); err != nil {
		return nil, err
	}

	impl := sonnx.NewOnlineRecognizer(&cfg)
	if impl == nil {
		return nil, core.Errorf(core.CodeInternal,
			"sherpa-onnx refused to create a streaming recogniser from model %s", m.ID)
	}

	caps := fam.Capabilities()
	// Measured, not declared, exactly as for the offline models: whether a
	// streaming model writes punctuation is a property of its vocabulary. No
	// Russian streaming model does, which is why a realtime transcript arrives
	// in lower case and the dialect says so.
	caps.PunctuationBuiltin = asr.VocabularyPunctuates(tokens)

	o := &Online{
		impl:         impl,
		modelID:      m.Key(),
		sampleRate:   m.FeatureSampleRate(),
		modelingUnit: m.ModelingUnit,
		caps:         caps,
	}
	runtime.SetFinalizer(o, func(o *Online) { _ = o.Close() })

	if !opt.SkipWarmup {
		if err := o.warmUp(ctx); err != nil {
			_ = o.Close()
			return nil, err
		}
	}
	return o, nil
}

// warmUp decodes a second of silence so onnxruntime resolves its kernels before
// a caller is listening. On the streaming path this matters more than on the
// offline one: the cost would otherwise land on the first connection, inside
// the latency budget the whole feature exists to meet.
func (o *Online) warmUp(ctx context.Context) error {
	s, err := o.NewStream()
	if err != nil {
		return err
	}
	defer s.Close()

	s.Accept(o.sampleRate, make([]float32, o.sampleRate))
	s.Finish()
	for s.Ready() {
		if err := ctx.Err(); err != nil {
			return err
		}
		o.Decode([]asr.Stream{s})
	}
	return nil
}

// NewStream starts one recognition stream.
func (o *Online) NewStream() (asr.Stream, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return nil, core.Errorf(core.CodeModelUnavailable, "streaming recogniser is closed")
	}
	impl := sonnx.NewOnlineStream(o.impl)
	if impl == nil {
		return nil, core.Errorf(core.CodeInternal, "sherpa-onnx refused to create a stream")
	}
	s := &onlineStream{owner: o, impl: impl}
	runtime.SetFinalizer(s, func(s *onlineStream) { s.Close() })
	return s, nil
}

// Decode advances a batch of streams. Streams that are closed, or that belong
// to another recogniser, are skipped rather than crashing the process.
func (o *Online) Decode(streams []asr.Stream) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		return
	}

	o.batch = o.batch[:0]
	for _, s := range streams {
		if st, ok := s.(*onlineStream); ok && st.owner == o && st.impl != nil {
			o.batch = append(o.batch, st.impl)
		}
	}
	switch len(o.batch) {
	case 0:
		return
	case 1:
		// DecodeStreams with one stream works, but the single-stream call is
		// the path sherpa-onnx optimises and it avoids building the C array.
		o.impl.Decode(o.batch[0])
	default:
		o.impl.DecodeStreams(o.batch)
	}
}

func (o *Online) SampleRate() int                 { return o.sampleRate }
func (o *Online) ModelID() string                 { return o.modelID }
func (o *Online) ModelingUnit() string            { return o.modelingUnit }
func (o *Online) Capabilities() core.Capabilities { return o.caps }

// Close releases the native recogniser. Streams must be closed first: they
// point into it.
func (o *Online) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	sonnx.DeleteOnlineRecognizer(o.impl)
	o.impl = nil
	runtime.SetFinalizer(o, nil)
	return nil
}

// onlineStream is the per-connection half.
//
// It holds no lock of its own: every method is documented as belonging to the
// one goroutine that owns the stream (asr.Stream). What it does take is the
// recogniser's read lock, so that a shutdown racing a decoder loop ends in a
// no-op rather than a call through a freed pointer.
type onlineStream struct {
	owner *Online
	impl  *sonnx.OnlineStream
}

func (s *onlineStream) Accept(sampleRate int, samples []float32) {
	if len(samples) == 0 {
		return
	}
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil || s.owner.closed {
		return
	}
	// AcceptWaveform resamples internally when the rate differs from the
	// feature extractor's, which is what lets a browser send 48 kHz and a
	// telephony bridge send 8 kHz to the same 8 kHz model.
	s.impl.AcceptWaveform(sampleRate, samples)
}

func (s *onlineStream) Ready() bool {
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil || s.owner.closed {
		return false
	}
	return s.owner.impl.IsReady(s.impl)
}

func (s *onlineStream) Hypothesis() asr.Hypothesis {
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil || s.owner.closed {
		return asr.Hypothesis{}
	}
	res := s.owner.impl.GetResult(s.impl)
	if res == nil {
		return asr.Hypothesis{}
	}
	return asr.Hypothesis{Text: res.Text, Tokens: res.Tokens, Timestamps: res.Timestamps}
}

func (s *onlineStream) Endpoint() bool {
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil || s.owner.closed {
		return false
	}
	return s.owner.impl.IsEndpoint(s.impl)
}

func (s *onlineStream) Reset() {
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil || s.owner.closed {
		return
	}
	s.owner.impl.Reset(s.impl)
}

func (s *onlineStream) Finish() {
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil || s.owner.closed {
		return
	}
	s.impl.InputFinished()
}

func (s *onlineStream) Close() {
	s.owner.mu.RLock()
	defer s.owner.mu.RUnlock()
	if s.impl == nil {
		return
	}
	// The recogniser owns the memory the stream points into, so a stream
	// outliving its recogniser must not be freed: Online.Close has already
	// released everything behind it.
	if !s.owner.closed {
		sonnx.DeleteOnlineStream(s.impl)
	}
	s.impl = nil
	runtime.SetFinalizer(s, nil)
}

// checkOnlineFilesExist turns a missing weight file into a clear error here
// instead of an opaque failure inside the C++ loader.
func checkOnlineFilesExist(m registry.Manifest, cfg sonnx.OnlineModelConfig, tokens string) error {
	paths := []string{
		tokens,
		cfg.Transducer.Encoder, cfg.Transducer.Decoder, cfg.Transducer.Joiner,
		cfg.Paraformer.Encoder, cfg.Paraformer.Decoder,
		cfg.Zipformer2Ctc.Model, cfg.NemoCtc.Model, cfg.ToneCtc.Model,
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return core.Errorf(core.CodeModelNotFound,
				"model %s: file %s is missing or unreadable", m.ID, p).WithCause(err)
		}
	}
	return nil
}

var _ asr.Streaming = (*Online)(nil)
