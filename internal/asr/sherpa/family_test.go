package sherpa

import (
	"strings"
	"testing"

	sonnx "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"github.com/usunrise88/nanoasr/internal/registry"
)

func manifest(family string, files map[string]string) registry.Manifest {
	return registry.Manifest{
		ID: "m", Revision: "1", Family: family, SampleRate: 16000,
		ModelingUnit: "bpe", Files: files,
		Features: registry.Features{SampleRate: 16000, Dim: 64},
	}
}

// Each family must put its weights in its own field of the sherpa-onnx config.
// Getting this wrong loads nothing and reports nothing, so it is checked
// directly rather than only through an end-to-end run — which also covers the
// transducer mapping, for which no golden transcript exists yet.
func TestFamilyConfigureTargetsTheRightField(t *testing.T) {
	single := map[string]string{"model": "model.int8.onnx", "tokens": "tokens.txt"}

	cases := []struct {
		family string
		files  map[string]string
		got    func(sonnx.OfflineModelConfig) string
	}{
		{"nemo_ctc", single, func(c sonnx.OfflineModelConfig) string { return c.NemoCTC.Model }},
		{"zipformer_ctc", single, func(c sonnx.OfflineModelConfig) string { return c.ZipformerCtc.Model }},
		{"wenet_ctc", single, func(c sonnx.OfflineModelConfig) string { return c.WenetCtc.Model }},
		{"telespeech", single, func(c sonnx.OfflineModelConfig) string { return c.TeleSpeechCtc }},
		{
			family: "transducer",
			files: map[string]string{
				"encoder": "encoder.onnx", "decoder": "decoder.onnx",
				"joiner": "joiner.onnx", "tokens": "tokens.txt",
			},
			got: func(c sonnx.OfflineModelConfig) string { return c.Transducer.Encoder },
		},
	}

	for _, c := range cases {
		t.Run(c.family, func(t *testing.T) {
			fam, err := LookupFamily(c.family)
			if err != nil {
				t.Fatal(err)
			}
			m := manifest(c.family, c.files)
			if err := fam.Validate(m.Files); err != nil {
				t.Fatalf("Validate: %v", err)
			}

			var cfg sonnx.OfflineModelConfig
			if err := fam.Configure(m, "/models/m", &cfg); err != nil {
				t.Fatalf("Configure: %v", err)
			}

			want := "/models/m/" + firstOf(c.files, "model", "encoder")
			if got := c.got(cfg); got != want {
				t.Errorf("configured path = %q, want %q", got, want)
			}
			if !fam.Capabilities().WordTimestamps {
				t.Error("every v1 family must be able to produce word timestamps")
			}
			// CTC decoders in sherpa-onnx return timestamps but no log
			// probabilities; only the transducer families may claim confidence.
			if wantConfidence := c.family == "transducer"; fam.Capabilities().Confidence != wantConfidence {
				t.Errorf("Confidence = %v, want %v", fam.Capabilities().Confidence, wantConfidence)
			}
		})
	}
}

func TestFamilyValidateNamesTheMissingFile(t *testing.T) {
	fam, err := LookupFamily("transducer")
	if err != nil {
		t.Fatal(err)
	}
	err = fam.Validate(map[string]string{"encoder": "e.onnx", "tokens": "t.txt"})
	if err == nil {
		t.Fatal("expected an error for an incomplete transducer manifest")
	}
	if !strings.Contains(err.Error(), "decoder") {
		t.Errorf("error should name the missing role, got %q", err)
	}
}

func TestLookupUnknownFamilyListsKnownOnes(t *testing.T) {
	_, err := LookupFamily("whisper")
	if err == nil {
		t.Fatal("whisper is not a v1 family; lookup should fail")
	}
	if !strings.Contains(err.Error(), "nemo_ctc") {
		t.Errorf("error should list the known families, got %q", err)
	}
}

func TestLoaderReportsMissingWeights(t *testing.T) {
	load := NewLoader(LoaderOptions{NumThreads: 1, SkipWarmup: true})
	m := manifest("nemo_ctc", map[string]string{"model": "model.onnx", "tokens": "tokens.txt"})

	_, err := load(t.Context(), m, t.TempDir())
	if err == nil {
		t.Fatal("expected an error when the weights are absent")
	}
	// The message has to name the file: "failed to create recognizer" from the
	// C++ side is not something an operator can act on.
	if !strings.Contains(err.Error(), "model.onnx") && !strings.Contains(err.Error(), "tokens.txt") {
		t.Errorf("error should name the missing file, got %q", err)
	}
}

func firstOf(files map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := files[k]; v != "" {
			return v
		}
	}
	return ""
}
