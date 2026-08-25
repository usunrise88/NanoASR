package asr

import (
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Modeling units, as they appear in a manifest.
const (
	UnitBPE        = "bpe"
	UnitChar       = "char"
	UnitCJKChar    = "cjkchar"
	UnitCJKCharBPE = "cjkchar+bpe"
)

// Hotwords is a caller's bias list, before it has been rendered for a model.
type Hotwords struct {
	Words []string
	Score float32
}

// Buffer renders the list the way sherpa-onnx expects to read it: one phrase
// per line, tokenised in the model's own modeling unit.
//
// The unit matters because sherpa-onnx looks the phrase up in the model's
// vocabulary. A character model wants the characters spaced apart; a subword
// model wants the phrase as written and does its own segmentation. Getting this
// wrong does not fail loudly — it produces a bias list that matches nothing.
func (h Hotwords) Buffer(modelingUnit string) (string, error) {
	var lines []string
	for _, w := range h.Words {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if strings.ContainsAny(w, "\n\r") {
			return "", core.Errorf(core.CodeInvalidRequest,
				"hotword %q contains a line break", w).WithParam("hotwords")
		}
		// cjkchar looks each character up separately, so the phrase is written
		// with the characters apart. Everything else sherpa-onnx will accept
		// segments the phrase itself.
		if modelingUnit == UnitCJKChar {
			lines = append(lines, spaceOutRunes(w))
			continue
		}
		lines = append(lines, w)
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n"), nil
}

// spaceOutRunes writes each rune separated by a space, which is how a cjkchar
// vocabulary spells a phrase.
func spaceOutRunes(s string) string {
	var b strings.Builder
	for i, r := range []rune(s) {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// HotwordsSupport says whether a model can be biased at all, and why not when
// it cannot.
//
// Every condition here is sherpa-onnx's, and each one was measured rather than
// assumed. This matters more than usual: sherpa-onnx does not decline a
// configuration it dislikes, it aborts the process. utils.cc:EncodeHotwords
// calls SHERPA_ONNX_LOGE and exits on an unsupported modeling unit, which takes
// the whole server down with the request. Nothing that fails this check may
// reach the loader.
func HotwordsSupport(family, modelingUnit, decodingMethod string, hasBPEVocab bool) error {
	if family != "transducer" {
		return core.Errorf(core.CodeCapabilityUnavailable,
			"hotword biasing works on transducer models; this one is %s", family)
	}
	if decodingMethod != "modified_beam_search" {
		return core.Errorf(core.CodeCapabilityUnavailable,
			"hotword biasing applies during beam search; this model decodes with %s",
			decodingMethod)
	}
	switch modelingUnit {
	case UnitCJKChar:
		// The only unit that needs no companion file: each character is a
		// token and the tokens file is enough to look them up.
		return nil
	case UnitBPE, UnitCJKCharBPE:
		if !hasBPEVocab {
			return core.Errorf(core.CodeCapabilityUnavailable,
				"hotword biasing needs a bpe_vocab file to tokenise phrases with, "+
					"and this model does not ship one")
		}
		return nil
	default:
		// Measured: sherpa-onnx accepts bpe, cjkchar and cjkchar+bpe here and
		// nothing else. A character vocabulary looks like it ought to work —
		// the characters are the tokens — but EncodeHotwords rejects it by
		// name and kills the process.
		return core.Errorf(core.CodeCapabilityUnavailable,
			"sherpa-onnx tokenises hotwords for bpe, cjkchar and cjkchar+bpe models only, "+
				"and this model's vocabulary is %q", modelingUnit)
	}
}

// Decoding methods sherpa-onnx accepts.
const (
	GreedySearch       = "greedy_search"
	ModifiedBeamSearch = "modified_beam_search"
)

// DecodingSupport reports whether a family can decode with the given method.
//
// Measured, and it matters more than it looks: an offline CTC recogniser told
// to use modified_beam_search does not decline it. It prints
//
//	offline-recognizer-ctc-impl.h:Init:207 Only greedy_search is supported
//
// and terminates the process. Since decoding_method is a request parameter,
// that turned one HTTP call into a way to kill the server, so nothing may
// reach the loader without passing this first.
func DecodingSupport(family, method string) error {
	switch method {
	case "", GreedySearch:
		// Every family can decode greedily.
		return nil
	case ModifiedBeamSearch:
		if family != "transducer" {
			return core.Errorf(core.CodeCapabilityUnavailable,
				"modified_beam_search works on transducer models; %s decodes greedily only",
				family)
		}
		return nil
	default:
		return core.Errorf(core.CodeCapabilityUnavailable,
			"unknown decoding method %q", method)
	}
}
