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
func NewLoader(opt LoaderOptions) func(context.Context, registry.Manifest, string, asr.Variant) (asr.Recognizer, error) {
	if opt.Provider == "" {
		opt.Provider = "cpu"
	}
	if opt.NumThreads < 1 {
		opt.NumThreads = 1
	}

	return func(ctx context.Context, m registry.Manifest, dir string, v asr.Variant) (asr.Recognizer, error) {
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
			DecodingMethod: decodingMethod(m, v),
			MaxActivePaths: maxActivePaths(m, v),
			BlankPenalty:   m.Runtime.BlankPenalty,
		}

		// Hotwords reach sherpa-onnx as a file. The offline recogniser config
		// has no in-memory variant — HotwordsBuf exists only on the streaming
		// struct — so one has to be written.
		//
		// It lives for the length of the load, not of a request: the phrases
		// are parsed into the decoder's context graph while the recogniser is
		// being constructed, and nothing reads the file afterwards. Writing it
		// per load rather than per request also means the temp directory does
		// not accumulate a file per call.
		if v.Hotwords != "" {
			// The caller is expected to have checked this already. Repeating it
			// here is not belt and braces: an unsupported unit does not produce
			// an error from sherpa-onnx, it terminates the process, so the last
			// thing standing between a bad variant and a dead server is this.
			if err := asr.HotwordsSupport(m.Family, m.ModelingUnit,
				cfg.DecodingMethod, cfg.ModelConfig.BpeVocab != ""); err != nil {
				return nil, err
			}
			path, cleanup, err := writeHotwords(v.Hotwords)
			if err != nil {
				return nil, err
			}
			defer cleanup()
			cfg.HotwordsFile = path
			cfg.HotwordsScore = v.HotwordsScore
		}

		// modeling_unit means two different things to two different readers.
		// Our word assembler always needs it, and gets it from the manifest.
		// sherpa-onnx only uses it to tokenise hotwords, and refuses to start
		// when told "bpe" without a vocabulary file to go with it — which is
		// most transducer releases.
		//
		// So it is passed down when the pair is complete, and also when the
		// unit needs no vocabulary file at all: for a character model the
		// characters are the tokens. That second case is the only reason
		// hotwords are reachable for the Russian models in the catalog.
		if vocab := m.OptionalFilePath(dir, "bpe_vocab"); vocab != "" {
			cfg.ModelConfig.ModelingUnit = m.ModelingUnit
			cfg.ModelConfig.BpeVocab = vocab
		} else if v.Hotwords != "" && m.ModelingUnit == asr.UnitCJKChar {
			// cjkchar is the one unit sherpa-onnx can tokenise hotwords in
			// without a companion vocabulary file.
			cfg.ModelConfig.ModelingUnit = m.ModelingUnit
		}

		if err := fam.Configure(m, dir, &cfg.ModelConfig); err != nil {
			return nil, err
		}
		if err := checkFilesExist(m, cfg.ModelConfig, tokens); err != nil {
			return nil, err
		}

		// The family says what this kind of model can do; the vocabulary says
		// whether this particular set of weights punctuates. Two releases of
		// the same family differ on exactly that, so it cannot come from the
		// family, and it is measured rather than declared.
		caps := fam.Capabilities()
		caps.PunctuationBuiltin = asr.VocabularyPunctuates(tokens)

		rec, err := New(&cfg, caps, m.ModelingUnit)
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

// decodingMethod is the request's choice over the manifest's. Before M5 the
// request's choice was parsed, stored and then dropped here.
func decodingMethod(m registry.Manifest, v asr.Variant) string {
	if v.DecodingMethod != "" {
		return v.DecodingMethod
	}
	if m.Runtime.DecodingMethod != "" {
		return m.Runtime.DecodingMethod
	}
	return "greedy_search"
}

func maxActivePaths(m registry.Manifest, v asr.Variant) int {
	if v.MaxActivePaths > 0 {
		return v.MaxActivePaths
	}
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

// writeHotwords materialises a bias list for sherpa-onnx to read during
// construction, and returns the cleanup that removes it again.
//
// 0600 in the process's temp directory: a hotword list is the caller's
// vocabulary — names, account numbers, product codes — and there is no reason
// for it to be world-readable even for the moment it exists.
func writeHotwords(buf string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "nanoasr-hotwords-*.txt")
	if err != nil {
		return "", func() {}, core.Errorf(core.CodeInternal,
			"cannot write the hotwords file").WithCause(err)
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, core.Errorf(core.CodeInternal,
			"cannot secure the hotwords file").WithCause(err)
	}
	if _, err := f.WriteString(buf + "\n"); err != nil {
		f.Close()
		cleanup()
		return "", func() {}, core.Errorf(core.CodeInternal,
			"cannot write the hotwords file").WithCause(err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, core.Errorf(core.CodeInternal,
			"cannot close the hotwords file").WithCause(err)
	}
	return name, cleanup, nil
}
