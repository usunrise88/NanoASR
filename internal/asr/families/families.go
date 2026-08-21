// Package families adapts sherpa-onnx model families to NanoASR manifests.
//
// Adding support for a family is adding one file here (SPEC §7.2): declare the
// files it needs and what it can produce. Nothing else in the codebase changes.
package families

import (
	"fmt"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

func init() {
	asr.RegisterFamily(transducer{})
	asr.RegisterFamily(ctc{name: "zipformer_ctc"})
	asr.RegisterFamily(ctc{name: "nemo_ctc"})
	asr.RegisterFamily(ctc{name: "wenet_ctc"})
	asr.RegisterFamily(ctc{name: "telespeech"})
}

// transducer covers zipformer transducers and RNNT exports such as GigaAM v2.
// It is the reference family for the telephony workload: token timestamps,
// per-token durations, hotword biasing and beam search.
type transducer struct{}

func (transducer) Name() string { return "transducer" }

func (transducer) Validate(files map[string]string) error {
	return require(files, "encoder", "decoder", "joiner", "tokens")
}

func (transducer) Capabilities() core.Capabilities {
	return core.Capabilities{WordTimestamps: true, Confidence: true}
}

// ctc covers the single-model CTC families. They are cheaper on CPU than a
// transducer and still carry frame-aligned timestamps, which also makes them
// the natural base for forced alignment later (SPEC §17, M6).
type ctc struct{ name string }

func (c ctc) Name() string { return c.name }

func (c ctc) Validate(files map[string]string) error {
	return require(files, "model", "tokens")
}

func (ctc) Capabilities() core.Capabilities {
	return core.Capabilities{WordTimestamps: true, Confidence: true}
}

func require(files map[string]string, keys ...string) error {
	for _, k := range keys {
		if files[k] == "" {
			return core.Errorf(core.CodeInvalidRequest,
				"manifest is missing required file %q", k).
				WithCause(fmt.Errorf("files: %v", files))
		}
	}
	return nil
}
