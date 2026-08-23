package sherpa

import (
	"context"
	"os"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// LoaderOptions are the process-wide defaults a model inherits when its
// manifest does not override them.
type LoaderOptions struct {
	// Provider is the onnxruntime execution provider. Only "cpu" is supported;
	// the field exists because the binding takes one and a future GPU build
	// would set it here and nowhere else.
	Provider string
	// NumThreads is the per-instance intra-op thread count. The CPU governor
	// admits work in units of this number, so the two must agree.
	NumThreads int
	Debug      bool
	// SkipWarmup disables the warm-up decode. Only tests should set it: the
	// first real request on a cold model is several times slower, which skews
	// every latency number that follows.
	SkipWarmup bool
}

// NewLoader returns a pool.Loader that materialises a sherpa-onnx recogniser
// from a manifest. The signature is structural rather than importing
// internal/pool, so cgo stays out of the pool's dependency graph.
func NewLoader(opt LoaderOptions) func(context.Context, registry.Manifest, string) (asr.Recognizer, error) {
	if opt.Provider == "" {
		opt.Provider = "cpu"
	}
	if opt.NumThreads < 1 {
		opt.NumThreads = 1
	}

	return func(ctx context.Context, m registry.Manifest, dir string) (asr.Recognizer, error) {
		if kind := m.EffectiveKind(); kind != registry.KindASR {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"model %s is a %s model and cannot be used for transcription", m.ID, kind)
		}

		fam, err := LookupFamily(m.Family)
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

		cfg := sonnx.OfflineRecognizerConfig{
			FeatConfig: sonnx.FeatureConfig{
				SampleRate: m.FeatureSampleRate(),
				FeatureDim: m.Features.Dim,
			},
			ModelConfig: sonnx.OfflineModelConfig{
				Tokens:     tokens,
				NumThreads: numThreads(m, opt),
				Provider:   opt.Provider,
				ModelType:  m.Runtime.ModelType,
				Debug:      boolToInt(opt.Debug),
			},
			DecodingMethod: decodingMethod(m),
			MaxActivePaths: maxActivePaths(m),
			BlankPenalty:   m.Runtime.BlankPenalty,
		}

		// modeling_unit means two different things to two different readers.
		// Our word assembler always needs it, and gets it from the manifest.
		// sherpa-onnx only uses it to tokenise hotwords, and refuses to start
		// when told "bpe" without a vocabulary file to go with it — which is
		// most transducer releases. So it is passed down only when the pair is
		// complete.
		if vocab := m.OptionalFilePath(dir, "bpe_vocab"); vocab != "" {
			cfg.ModelConfig.ModelingUnit = m.ModelingUnit
			cfg.ModelConfig.BpeVocab = vocab
		}

		if err := fam.Configure(m, dir, &cfg.ModelConfig); err != nil {
			return nil, err
		}
		if err := checkFilesExist(m, cfg.ModelConfig, tokens); err != nil {
			return nil, err
		}

		rec, err := New(&cfg, fam.Capabilities(), m.ModelingUnit)
		if err != nil {
			return nil, err
		}

		if !opt.SkipWarmup {
			if err := warmUp(ctx, rec, m.FeatureSampleRate()); err != nil {
				_ = rec.Close()
				return nil, err
			}
		}
		return rec, nil
	}
}

// warmUp decodes one second of silence so onnxruntime allocates its arenas and
// resolves its kernels before a user is waiting on the result.
func warmUp(ctx context.Context, rec *Recognizer, sampleRate int) error {
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	if _, err := rec.Decode(ctx, [][]float32{make([]float32, sampleRate)}, sampleRate); err != nil {
		return core.Errorf(core.CodeInternal, "model failed its warm-up decode").WithCause(err)
	}
	return nil
}

// checkFilesExist turns a missing weight file into a clear error here instead
// of an opaque failure inside the C++ loader.
func checkFilesExist(m registry.Manifest, cfg sonnx.OfflineModelConfig, tokens string) error {
	paths := []string{
		tokens,
		cfg.Transducer.Encoder, cfg.Transducer.Decoder, cfg.Transducer.Joiner,
		cfg.NemoCTC.Model, cfg.ZipformerCtc.Model, cfg.WenetCtc.Model, cfg.TeleSpeechCtc,
		cfg.BpeVocab,
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

func numThreads(m registry.Manifest, opt LoaderOptions) int {
	if m.Runtime.NumThreads > 0 {
		return m.Runtime.NumThreads
	}
	return opt.NumThreads
}

func decodingMethod(m registry.Manifest) string {
	if m.Runtime.DecodingMethod != "" {
		return m.Runtime.DecodingMethod
	}
	return "greedy_search"
}

func maxActivePaths(m registry.Manifest) int {
	if m.Runtime.MaxActivePaths > 0 {
		return m.Runtime.MaxActivePaths
	}
	return 4
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
