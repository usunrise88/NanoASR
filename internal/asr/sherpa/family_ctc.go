package sherpa

import (
	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

func init() {
	// Every CTC family is one .onnx plus tokens; they differ only in which
	// field of the sherpa-onnx config the path belongs to.
	RegisterFamily(ctc{
		name:   "nemo_ctc",
		assign: func(p string, c *sonnx.OfflineModelConfig) { c.NemoCTC.Model = p },
	})
	RegisterFamily(ctc{
		name:   "zipformer_ctc",
		assign: func(p string, c *sonnx.OfflineModelConfig) { c.ZipformerCtc.Model = p },
	})
	RegisterFamily(ctc{
		name:   "wenet_ctc",
		assign: func(p string, c *sonnx.OfflineModelConfig) { c.WenetCtc.Model = p },
	})
	RegisterFamily(ctc{
		name:   "telespeech",
		assign: func(p string, c *sonnx.OfflineModelConfig) { c.TeleSpeechCtc = p },
	})
}

// ctc is the single-model CTC shape. These are cheaper on CPU than a
// transducer, still carry frame-aligned timestamps, and are the natural base
// for forced alignment later.
type ctc struct {
	name   string
	assign func(modelPath string, cfg *sonnx.OfflineModelConfig)
}

func (c ctc) Name() string { return c.name }

func (ctc) Validate(files map[string]string) error {
	return requireFiles(files, "model", "tokens")
}

func (ctc) Capabilities() core.Capabilities {
	// No confidence: sherpa-onnx's CTC decoders return timestamps but leave
	// ys_log_probs empty, verified against GigaAM v2. Claiming otherwise would
	// advertise a field that is always absent.
	return core.Capabilities{WordTimestamps: true}
}

func (c ctc) Configure(m registry.Manifest, dir string, cfg *sonnx.OfflineModelConfig) error {
	model, err := m.FilePath(dir, "model")
	if err != nil {
		return err
	}
	c.assign(model, cfg)
	return nil
}
