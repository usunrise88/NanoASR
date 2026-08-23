package registry

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTokens(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.txt")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The modeling unit is the field that fails quietly: a character vocabulary
// read as SentencePiece produces one word per character, with word timings to
// match.
func TestDetectModelingUnit(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			// GigaAM's Russian vocabulary: a literal space, then letters.
			name:  "russian characters",
			lines: []string{"  0", "а 1", "б 2", "в 3", "г 4", "д 5", "<blk> 6"},
			want:  "char",
		},
		{
			name:  "sentencepiece",
			lines: []string{"<blk> 0", "▁the 1", "▁of 2", "ing 3", "ed 4", "▁a 5"},
			want:  "bpe",
		},
		{
			name:  "chinese characters",
			lines: []string{"<blk> 0", "今 1", "天 2", "好 3", "我 4", "你 5"},
			want:  "cjkchar",
		},
		{
			name:  "chinese with sentencepiece english",
			lines: []string{"<blk> 0", "今 1", "天 2", "好 3", "我 4", "▁the 5", "▁of 6"},
			want:  "cjkchar+bpe",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, note, err := detectModelingUnit(writeTokens(t, c.lines...))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("unit = %q, want %q (%s)", got, c.want, note)
			}
			if note == "" {
				t.Error("every inference must come with a note a reviewer can check")
			}
		})
	}
}

func TestDetectModelingUnitRejectsEmptyVocabulary(t *testing.T) {
	if _, _, err := detectModelingUnit(writeTokens(t, "<blk> 0")); err == nil {
		t.Fatal("a vocabulary of nothing but special tokens should be an error")
	}
}

func TestClassifyFiles(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  map[string]string
	}{
		{
			name:  "transducer",
			files: []string{"encoder.onnx", "decoder.onnx", "joiner.onnx", "tokens.txt"},
			want: map[string]string{
				"encoder": "encoder.onnx", "decoder": "decoder.onnx",
				"joiner": "joiner.onnx", "tokens": "tokens.txt",
			},
		},
		{
			name:  "single model",
			files: []string{"model.onnx", "tokens.txt"},
			want:  map[string]string{"model": "model.onnx", "tokens": "tokens.txt"},
		},
		{
			// A CPU deployment should run the quantised weights, and listing
			// both in one manifest would be ambiguous.
			name:  "prefers int8",
			files: []string{"model.onnx", "model.int8.onnx", "tokens.txt"},
			want:  map[string]string{"model": "model.int8.onnx", "tokens": "tokens.txt"},
		},
		{
			name:  "sentencepiece vocabulary",
			files: []string{"encoder.onnx", "decoder.onnx", "joiner.onnx", "tokens.txt", "bpe.model"},
			want: map[string]string{
				"encoder": "encoder.onnx", "decoder": "decoder.onnx", "joiner": "joiner.onnx",
				"tokens": "tokens.txt", "bpe_vocab": "bpe.model",
			},
		},
		{
			// An encoder without a joiner is not a transducer; it must not be
			// classified as one on the strength of a file name.
			name:  "encoder without joiner",
			files: []string{"encoder.onnx", "tokens.txt"},
			want:  map[string]string{"model": "encoder.onnx", "tokens": "tokens.txt"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := classifyFiles(c.files)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for role, name := range c.want {
				if got[role] != name {
					t.Errorf("role %q = %q, want %q", role, got[role], name)
				}
			}
		})
	}
}

func TestDetectFamily(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		meta  Metadata
		want  string
	}{
		{"joiner means transducer", map[string]string{"joiner": "j.onnx"}, Metadata{"model_type": "EncDecCTCModel"}, "transducer"},
		{"nemo", map[string]string{"model": "m.onnx"}, Metadata{"model_type": "EncDecCTCModel"}, "nemo_ctc"},
		{"zipformer", map[string]string{"model": "m.onnx"}, Metadata{"model_type": "zipformer2_ctc"}, "zipformer_ctc"},
		{"wenet", map[string]string{"model": "m.onnx"}, Metadata{"model_type": "wenet_ctc"}, "wenet_ctc"},
		{"unknown defaults", map[string]string{"model": "m.onnx"}, Metadata{}, "nemo_ctc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectFamily(c.files, c.meta); got != c.want {
				t.Errorf("family = %q, want %q", got, c.want)
			}
		})
	}
}

// metadataEntry encodes a StringStringEntryProto: field 1 key, field 2 value.
func metadataEntry(key, value string) []byte {
	out := []byte{0x0A, byte(len(key))}
	out = append(out, key...)
	out = append(out, 0x12)
	var l [binary.MaxVarintLen64]byte
	out = append(out, l[:binary.PutUvarint(l[:], uint64(len(value)))]...)
	return append(out, value...)
}

func TestParseMetadataReadsProtobufEntries(t *testing.T) {
	var buf []byte
	buf = append(buf, []byte("some preceding graph bytes")...)
	buf = append(buf, metadataEntry("model_type", "EncDecCTCModel")...)
	buf = append(buf, metadataEntry("vocab_size", "34")...)
	buf = append(buf, metadataEntry("language", "Russian")...)
	buf = append(buf, metadataEntry("is_giga_am", "1")...)

	got := parseMetadata(buf)
	for key, want := range map[string]string{
		"model_type": "EncDecCTCModel", "vocab_size": "34",
		"language": "Russian", "is_giga_am": "1",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["license"]; ok {
		t.Error("parseMetadata invented an absent key")
	}
}

// A key name can occur in the graph as a tensor name; only a real entry counts.
func TestParseMetadataIgnoresLooseKeyStrings(t *testing.T) {
	buf := append([]byte("model_type appears here as plain text"), metadataEntry("vocab_size", "12")...)

	got := parseMetadata(buf)
	if v, ok := got["model_type"]; ok {
		t.Errorf("model_type = %q from a string that is not an entry", v)
	}
	if got["vocab_size"] != "12" {
		t.Errorf("vocab_size = %q, want 12", got["vocab_size"])
	}
}

func TestLanguageCode(t *testing.T) {
	for name, want := range map[string]string{
		"Russian": "ru", "english": "en", "Chinese": "zh", "Klingon": "klingon", " German ": "de",
	} {
		if got := languageCode(name); got != want {
			t.Errorf("languageCode(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInspectDraftsAUsableManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sherpa-onnx-test-model-ru")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	model := append([]byte("fake onnx graph"), metadataEntry("model_type", "EncDecCTCModel")...)
	model = append(model, metadataEntry("language", "Russian")...)
	model = append(model, metadataEntry("is_giga_am", "1")...)
	if err := os.WriteFile(filepath.Join(dir, "model.int8.onnx"), model, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tokens.txt"),
		[]byte("  0\nа 1\nб 2\nв 3\n<blk> 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	draft, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := draft.Manifest

	if m.ID != "test-model-ru" {
		t.Errorf("id = %q, want the directory name without the sherpa-onnx prefix", m.ID)
	}
	if m.Family != "nemo_ctc" || m.ModelingUnit != "char" {
		t.Errorf("family/unit = %q/%q, want nemo_ctc/char", m.Family, m.ModelingUnit)
	}
	if m.Features.Dim != 64 {
		t.Errorf("features.dim = %d, want 64 from is_giga_am", m.Features.Dim)
	}
	if len(m.Languages) != 1 || m.Languages[0] != "ru" {
		t.Errorf("languages = %v, want [ru]", m.Languages)
	}
	// The draft must be loadable as-is, or it is not a draft of anything.
	if err := m.Validate(); err != nil {
		t.Errorf("the drafted manifest does not validate: %v", err)
	}
	if len(draft.Notes) == 0 {
		t.Error("no notes: every inference should be checkable")
	}
}

func TestInspectRefusesADirectoryWithoutAModel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("nothing here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(dir); err == nil {
		t.Fatal("expected an error for a directory with no .onnx")
	}
}
