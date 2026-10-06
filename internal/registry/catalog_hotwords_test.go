package registry

import (
	"context"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
)

// Which catalog entries can carry a hotword dictionary, stated rather than
// derived.
//
// This is the table both READMEs print and the UI repeats, and the point of
// pinning it here is that it is the kind of claim that goes stale quietly: an
// entry gains a bpe_vocab, or a new model is added, and the documentation says
// something that stopped being true. A failure here means the documentation
// needs the same edit, not that the test needs relaxing.
func TestWhichCatalogModelsCanBeBiased(t *testing.T) {
	want := map[string]bool{
		"gigaam-v2-ctc-ru":             false, // CTC: nothing to bias during
		"gigaam-v3-ctc-punct-ru":       false, // CTC
		"gigaam-v3-rnnt-punct-ru":      false, // transducer, subwords, no vocabulary file
		"gigaam-v2-rnnt-ru":            false, // transducer, but a character vocabulary
		"t-one-ctc-ru":                 false, // streaming CTC
		"streaming-zipformer-small-ru": false, // streaming: biased at load, not per request
		"zipformer-small-en":           false, // transducer, subwords, no vocabulary file
	}

	entries, err := (&Local{}).Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, man := range entries {
		if man.EffectiveKind() != KindASR {
			continue
		}
		seen[man.ID] = true
		expected, known := want[man.ID]
		if !known {
			t.Errorf("catalog entry %q is new: add it to this table, to the hotword "+
				"table in both READMEs, and to the UI's note", man.ID)
			continue
		}
		got, reason := asr.HotwordsCapability(man.Family, man.ModelingUnit, man.Files["bpe_vocab"] != "")
		if got != expected {
			t.Errorf("%s: hotwords = %v (%s), documented as %v", man.ID, got, reason, expected)
		}
		if !got && reason == "" {
			t.Errorf("%s cannot be biased and does not say why", man.ID)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s is documented but no longer in the catalog", id)
		}
	}
}
