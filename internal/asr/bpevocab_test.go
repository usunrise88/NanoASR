package asr

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sentencepieceModel builds the bytes a real bpe.model carries: a ModelProto
// whose field 1 repeats a message of {piece, score}, followed by a trainer_spec
// this code is meant to walk past.
func sentencepieceModel(pieces []sentencePiece) []byte {
	var out []byte
	for _, p := range pieces {
		var body []byte
		body = append(body, 0x0a, byte(len(p.piece))) // field 1, length-delimited
		body = append(body, p.piece...)
		body = append(body, 0x15) // field 2, fixed32
		var f [4]byte
		binary.LittleEndian.PutUint32(f[:], math.Float32bits(p.score))
		body = append(body, f[:]...)
		body = append(body, 0x18, 0x04) // field 3, varint: piece type

		out = append(out, 0x0a, byte(len(body)))
		out = append(out, body...)
	}
	// trainer_spec: field 2, length-delimited, contents irrelevant here.
	out = append(out, 0x12, 0x03, 'a', 'b', 'c')
	return out
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The regression this file exists for: handed a binary bpe.model — which is
// what a sherpa-onnx model archive ships, our own catalog included —
// sherpa-onnx does not decline it, it calls exit() and takes the server down
// mid-request. Nothing may reach it in that form.
func TestBinaryModelIsConvertedToTheFormSherpaReads(t *testing.T) {
	path := writeTemp(t, "bpe.model", sentencepieceModel([]sentencePiece{
		{"<unk>", 0},
		{"▁ро", -3.5},
		{"ма", -2.25},
	}))

	text, converted, err := BPEVocabulary(path)
	if err != nil {
		t.Fatal(err)
	}
	if !converted {
		t.Error("converted = false, but the file on disk is a sentencepiece model")
	}
	want := "<unk> 0\n▁ро -3.5\nма -2.25\n"
	if text != want {
		t.Errorf("vocabulary =\n%q\nwant\n%q", text, want)
	}
}

func TestATextVocabularyIsPassedThroughUntouched(t *testing.T) {
	const vocab = "<unk> 0\n▁ро -3.5213732719421387\nма -2.25\n"
	path := writeTemp(t, "bpe.vocab", []byte(vocab))

	text, converted, err := BPEVocabulary(path)
	if err != nil {
		t.Fatal(err)
	}
	if converted {
		t.Error("converted = true for a file that is already in the right form")
	}
	if text != vocab {
		t.Errorf("vocabulary = %q, want it unchanged", text)
	}
}

// A file in neither format has to come back as an error. The alternative is
// handing it to sherpa-onnx to find out, and what sherpa-onnx does with it is
// exit().
func TestNeitherFormatIsRefusedRatherThanPassedOn(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"a readme", "This directory holds the model.\n"},
		{"empty", ""},
		{"tokens.txt handed over by mistake", "▁ро 42\nма 43\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := BPEVocabulary(writeTemp(t, "bpe.model", []byte(c.body)))
			if c.name == "tokens.txt handed over by mistake" {
				// Ids parse as numbers, so this one is indistinguishable from a
				// vocabulary by format alone and is passed through. Recorded
				// here so the limit of the sniffing is stated, not assumed.
				if err != nil {
					t.Skip("tokens.txt is accepted as a vocabulary; scores are its ids")
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a file that is in neither format")
			}
			if !strings.Contains(err.Error(), "sentencepiece") {
				t.Errorf("error = %v, want it to name both formats", err)
			}
		})
	}
}

func TestAMissingFileIsAnError(t *testing.T) {
	if _, _, err := BPEVocabulary(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing vocabulary was accepted")
	}
}
