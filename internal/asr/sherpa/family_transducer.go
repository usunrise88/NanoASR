package sherpa

import (
	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

func init() { RegisterFamily(transducer{}) }

// transducer covers zipformer transducers and RNNT exports such as GigaAM v2
// RNNT. It is the richest family: token timestamps with per-token durations,
// hotword biasing and beam search all come from here.
//
// Note for reviewers: M1 was validated against a CTC model, so this mapping is
// exercised by unit tests but has no golden transcript behind it yet. The first
// transducer added to the catalog closes that gap.
type transducer struct{}

func (transducer) Name() string { return "transducer" }

func (transducer) Validate(files map[string]string) error {
	return requireFiles(files, "encoder", "decoder", "joiner", "tokens")
}

func (transducer) Capabilities() core.Capabilities {
	// Confidence comes from ys_log_probs, which the transducer decoders fill.
	// Unverified against a real transducer model — see the note above.
	return core.Capabilities{WordTimestamps: true, Confidence: true}
}

func (transducer) Configure(m registry.Manifest, dir string, cfg *sonnx.OfflineModelConfig) error {
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

	cfg.Transducer = sonnx.OfflineTransducerModelConfig{
		Encoder: encoder,
		Decoder: decoder,
		Joiner:  joiner,
	}
	return nil
}
