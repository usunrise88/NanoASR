package sherpa

import (
	"fmt"
	"sort"
	"sync"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// OnlineFamily adapts one streaming sherpa-onnx model family to a manifest.
//
// It is a second interface beside Family rather than a method on it because the
// two configure different C structs — OnlineModelConfig has encoder fields that
// carry chunk and context sizes, OfflineModelConfig has whole-file ones — and a
// single interface would have had to take both and let each implementation
// ignore one. No model implements both: a streaming export and an offline
// export of the same architecture are different files.
type OnlineFamily interface {
	Name() string
	Validate(files map[string]string) error
	Capabilities() core.Capabilities
	Configure(m registry.Manifest, dir string, cfg *sonnx.OnlineModelConfig) error
}

var (
	onlineMu       sync.RWMutex
	onlineFamilies = map[string]OnlineFamily{}
)

// RegisterOnlineFamily is called from a family file's init().
func RegisterOnlineFamily(f OnlineFamily) {
	onlineMu.Lock()
	defer onlineMu.Unlock()
	if _, dup := onlineFamilies[f.Name()]; dup {
		panic(fmt.Sprintf("sherpa: online family %q registered twice", f.Name()))
	}
	onlineFamilies[f.Name()] = f
}

// LookupOnlineFamily returns the adapter for a streaming manifest family.
func LookupOnlineFamily(name string) (OnlineFamily, error) {
	onlineMu.RLock()
	defer onlineMu.RUnlock()
	f, ok := onlineFamilies[name]
	if !ok {
		// Worth the extra sentence: the two most likely mistakes are a
		// misspelling and pointing realtime.model at an ordinary model, and
		// they need different fixes.
		if _, offline := families[name]; offline {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"model family %q is not a streaming family: realtime needs a model trained "+
					"for streaming (known: %v)", name, onlineFamilyNamesLocked())
		}
		return nil, core.Errorf(core.CodeModelNotFound,
			"unknown streaming model family %q (known: %v)", name, onlineFamilyNamesLocked())
	}
	return f, nil
}

// OnlineFamilies lists registered streaming family names, sorted.
func OnlineFamilies() []string {
	onlineMu.RLock()
	defer onlineMu.RUnlock()
	return onlineFamilyNamesLocked()
}

func onlineFamilyNamesLocked() []string {
	out := make([]string, 0, len(onlineFamilies))
	for name := range onlineFamilies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func init() {
	// A streaming transducer: zipformer2 and the Vosk exports built from it.
	// The only streaming family with per-token timestamps worth reporting.
	RegisterOnlineFamily(onlineTransducer{})

	// The CTC shapes. Each is one .onnx plus tokens and differs only in which
	// field of the config the path belongs in — the same split as the offline
	// CTC families, for the same reason.
	RegisterOnlineFamily(onlineCTC{
		name:   "streaming_zipformer2_ctc",
		assign: func(p string, c *sonnx.OnlineModelConfig) { c.Zipformer2Ctc.Model = p },
	})
	RegisterOnlineFamily(onlineCTC{
		name:   "streaming_nemo_ctc",
		assign: func(p string, c *sonnx.OnlineModelConfig) { c.NemoCtc.Model = p },
	})
	// T-one: the Russian telephony streaming CTC model. sherpa-onnx gives it a
	// config field of its own because its encoder takes a fixed 300 ms chunk.
	RegisterOnlineFamily(onlineCTC{
		name:   "streaming_t_one_ctc",
		assign: func(p string, c *sonnx.OnlineModelConfig) { c.ToneCtc.Model = p },
	})
	RegisterOnlineFamily(onlineParaformer{})
}

// onlineTransducer is the encoder/decoder/joiner shape.
type onlineTransducer struct{}

func (onlineTransducer) Name() string { return "streaming_transducer" }

func (onlineTransducer) Validate(files map[string]string) error {
	return requireFiles(files, "encoder", "decoder", "joiner", "tokens")
}

func (onlineTransducer) Capabilities() core.Capabilities {
	// Timestamps yes, confidence no: the streaming result struct carries
	// tokens and timestamps and has no field for log probabilities, so unlike
	// the offline transducer there is nothing to derive confidence from.
	return core.Capabilities{WordTimestamps: true}
}

func (onlineTransducer) Configure(m registry.Manifest, dir string, cfg *sonnx.OnlineModelConfig) error {
	encoder, err := m.FilePath(dir, "encoder")
	if err != nil {
		return err
	}
	decoder, err := m.FilePath(dir, "decoder")
	if err != nil {
		return err
	}
	joiner, err := m.FilePath(dir, "joiner")
	if err != nil {
		return err
	}
	cfg.Transducer = sonnx.OnlineTransducerModelConfig{
		Encoder: encoder,
		Decoder: decoder,
		Joiner:  joiner,
	}
	return nil
}

// onlineCTC is the single-model streaming CTC shape.
type onlineCTC struct {
	name   string
	assign func(modelPath string, cfg *sonnx.OnlineModelConfig)
}

func (c onlineCTC) Name() string { return c.name }

func (onlineCTC) Validate(files map[string]string) error {
	return requireFiles(files, "model", "tokens")
}

func (onlineCTC) Capabilities() core.Capabilities {
	return core.Capabilities{WordTimestamps: true}
}

func (c onlineCTC) Configure(m registry.Manifest, dir string, cfg *sonnx.OnlineModelConfig) error {
	model, err := m.FilePath(dir, "model")
	if err != nil {
		return err
	}
	c.assign(model, cfg)
	return nil
}

// onlineParaformer is Chinese-only upstream, and is here because leaving it out
// would have been a decision about which languages this server can stream.
type onlineParaformer struct{}

func (onlineParaformer) Name() string { return "streaming_paraformer" }

func (onlineParaformer) Validate(files map[string]string) error {
	return requireFiles(files, "encoder", "decoder", "tokens")
}

func (onlineParaformer) Capabilities() core.Capabilities {
	return core.Capabilities{WordTimestamps: true}
}

func (onlineParaformer) Configure(m registry.Manifest, dir string, cfg *sonnx.OnlineModelConfig) error {
	encoder, err := m.FilePath(dir, "encoder")
	if err != nil {
		return err
	}
	decoder, err := m.FilePath(dir, "decoder")
	if err != nil {
		return err
	}
	cfg.Paraformer = sonnx.OnlineParaformerModelConfig{Encoder: encoder, Decoder: decoder}
	return nil
}
