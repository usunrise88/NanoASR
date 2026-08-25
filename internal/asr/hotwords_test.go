package asr

import (
	"strings"
	"testing"
)

func TestHotwordsBuffer(t *testing.T) {
	cases := []struct {
		name  string
		words []string
		unit  string
		want  string
	}{
		{
			// cjkchar looks each character up on its own.
			name:  "a cjkchar model gets its characters spaced",
			words: []string{"惊蛰"},
			unit:  UnitCJKChar,
			want:  "惊 蛰",
		},
		{
			name:  "a subword model gets the phrase as written",
			words: []string{"ромашка"},
			unit:  UnitBPE,
			want:  "ромашка",
		},
		{
			name:  "one phrase per line",
			words: []string{"ромашка", "васильки"},
			unit:  UnitBPE,
			want:  "ромашка\nвасильки",
		},
		{
			// A multi-word phrase stays one line: it is one thing to bias
			// towards, not two.
			name:  "a multi-word phrase stays on one line",
			words: []string{"общество с ограниченной ответственностью"},
			unit:  UnitBPE,
			want:  "общество с ограниченной ответственностью",
		},
		{
			name:  "blank entries are dropped rather than written as empty lines",
			words: []string{"ромашка", "   ", ""},
			unit:  UnitBPE,
			want:  "ромашка",
		},
		{
			name:  "nothing usable produces nothing",
			words: []string{"  "},
			unit:  UnitBPE,
			want:  "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Hotwords{Words: c.words}.Buffer(c.unit)
			if err != nil {
				t.Fatalf("Buffer: %v", err)
			}
			if got != c.want {
				t.Errorf("Buffer = %q, want %q", got, c.want)
			}
		})
	}
}

// A line break inside a hotword would silently become two hotwords, one of them
// probably nonsense. Rejecting by name is the house rule for bad request values.
func TestHotwordsBufferRejectsLineBreaks(t *testing.T) {
	_, err := Hotwords{Words: []string{"ромашка\nвасильки"}}.Buffer(UnitBPE)
	if err == nil {
		t.Fatal("a hotword containing a line break must be refused")
	}
	if !strings.Contains(err.Error(), "hotwords") {
		t.Errorf("error %q should name the offending field", err)
	}
}

func TestHotwordsSupport(t *testing.T) {
	cases := []struct {
		name     string
		family   string
		unit     string
		method   string
		hasVocab bool
		ok       bool
	}{
		{
			// Measured against sherpa-onnx, not assumed: a character vocabulary
			// looks like it should work and does not. EncodeHotwords rejects
			// the unit by name and terminates the process, so this case has to
			// be refused before it reaches the loader.
			name:   "a character vocabulary is refused rather than crashed on",
			family: "transducer", unit: UnitChar, method: "modified_beam_search", ok: false,
		},
		{
			name:   "cjkchar needs no companion vocabulary file",
			family: "transducer", unit: UnitCJKChar, method: "modified_beam_search", ok: true,
		},
		{
			name:   "transducer with beam search and a bpe vocabulary file",
			family: "transducer", unit: UnitBPE, method: "modified_beam_search",
			hasVocab: true, ok: true,
		},
		{
			name:   "a subword model with no vocabulary file cannot tokenise phrases",
			family: "transducer", unit: UnitBPE, method: "modified_beam_search",
			hasVocab: false, ok: false,
		},
		{
			name:   "greedy decoding has nowhere to apply a bias",
			family: "transducer", unit: UnitChar, method: "greedy_search", ok: false,
		},
		{
			name:   "ctc has no beam to bias",
			family: "nemo_ctc", unit: UnitChar, method: "modified_beam_search", ok: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := HotwordsSupport(c.family, c.unit, c.method, c.hasVocab)
			if c.ok && err != nil {
				t.Errorf("HotwordsSupport = %v, want supported", err)
			}
			if !c.ok && err == nil {
				t.Error("HotwordsSupport accepted a combination sherpa-onnx cannot bias")
			}
		})
	}
}

// The case that killed a server: sherpa-onnx does not decline
// modified_beam_search on a CTC model, it prints a line and calls exit().
// Since decoding_method is a request parameter, one HTTP call could take the
// process down.
func TestDecodingSupport(t *testing.T) {
	cases := []struct {
		name   string
		family string
		method string
		ok     bool
	}{
		{"transducer decodes with a beam", "transducer", ModifiedBeamSearch, true},
		{"transducer decodes greedily too", "transducer", GreedySearch, true},
		{"ctc has no beam and must be refused", "nemo_ctc", ModifiedBeamSearch, false},
		{"zipformer ctc likewise", "zipformer_ctc", ModifiedBeamSearch, false},
		{"ctc decodes greedily", "nemo_ctc", GreedySearch, true},
		{"an empty method is the model's own", "nemo_ctc", "", true},
		{"an unknown method is refused for every family", "transducer", "beam_search", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := DecodingSupport(c.family, c.method)
			if c.ok && err != nil {
				t.Errorf("DecodingSupport = %v, want supported", err)
			}
			if !c.ok && err == nil {
				t.Error("DecodingSupport accepted a combination that terminates the process")
			}
		})
	}
}
