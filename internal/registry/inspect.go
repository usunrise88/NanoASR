package registry

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Inspecting a model directory to draft its manifest.
//
// Three manifest fields cannot be read off a directory listing: the family, the
// modeling unit and the feature dimension. In M1 they were worked out by hand
// with `strings` over the .onnx tail and `cat -A tokens.txt`, which does not
// scale to a catalog that grows.
//
// Getting the modeling unit wrong is the one that fails quietly: a character
// vocabulary read as SentencePiece yields one word per character, with word
// timings to match.

// Draft is a proposed manifest plus what the inspection could not settle.
type Draft struct {
	Manifest Manifest
	// Notes explain every inference that was made, so a reviewer can check
	// them rather than trust them.
	Notes []string
	// Unresolved names fields the operator must confirm, most importantly
	// features.dim.
	Unresolved []string
}

// Metadata holds the ONNX metadata_props entries relevant to a manifest.
type Metadata map[string]string

// Inspect examines an unpacked model directory and drafts a manifest.
func Inspect(dir string) (Draft, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Draft{}, core.Errorf(core.CodeInvalidRequest,
			"cannot read model directory %s", dir).WithCause(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	d := Draft{Manifest: Manifest{
		ID:         suggestID(dir),
		Revision:   "1",
		Kind:       KindASR,
		SampleRate: 16000,
		Files:      map[string]string{},
	}}

	files, familyNote := classifyFiles(names)
	d.Manifest.Files = files
	if familyNote != "" {
		d.Notes = append(d.Notes, familyNote)
	}

	primary := files["model"]
	if primary == "" {
		primary = files["encoder"]
	}
	if primary == "" {
		return d, core.Errorf(core.CodeInvalidRequest,
			"%s contains no .onnx model", dir)
	}

	meta, err := ReadONNXMetadata(filepath.Join(dir, primary))
	if err != nil {
		d.Notes = append(d.Notes, "could not read ONNX metadata: "+err.Error())
	}

	d.Manifest.Family = detectFamily(files, meta)
	d.Notes = append(d.Notes, fmt.Sprintf("family %q from %s",
		d.Manifest.Family, familySource(files, meta)))

	if tokens := files["tokens"]; tokens != "" {
		unit, note, err := detectModelingUnit(filepath.Join(dir, tokens))
		if err != nil {
			d.Notes = append(d.Notes, "could not read tokens.txt: "+err.Error())
		} else {
			d.Manifest.ModelingUnit = unit
			d.Notes = append(d.Notes, note)
		}
	} else {
		d.Unresolved = append(d.Unresolved, "files.tokens: no tokens.txt found")
	}

	// features.dim is the one that fails silently, so it gets the loudest
	// handling: a known special case, or an explicit refusal to guess.
	switch {
	case meta["is_giga_am"] != "":
		d.Manifest.Features = Features{SampleRate: 16000, Dim: 64}
		d.Notes = append(d.Notes, "features.dim 64 because the model declares is_giga_am")
	default:
		d.Manifest.Features = Features{SampleRate: 16000}
		d.Unresolved = append(d.Unresolved,
			"features.dim: not stated in the model; 80 is the common value and 64 is "+
				"used by GigaAM. --probe reports whether this model uses the value at all")
	}

	if lang := meta["language"]; lang != "" {
		code := languageCode(lang)
		d.Manifest.Languages = []string{code}
		d.Notes = append(d.Notes, fmt.Sprintf("language from ONNX metadata: %s → %s", lang, code))
	} else {
		d.Unresolved = append(d.Unresolved, "languages: not stated in the model")
	}

	if v := meta["vocab_size"]; v != "" {
		d.Notes = append(d.Notes, "vocab_size from metadata: "+v)
	}
	if lic := meta["license"]; lic != "" {
		d.Manifest.License = lic
		d.Notes = append(d.Notes, "license URL from metadata: "+lic)
	} else {
		d.Unresolved = append(d.Unresolved,
			"license: read the LICENSE file in the directory; a catalog that offers "+
				"a download must state what it is offering")
	}

	return d, nil
}

// classifyFiles maps the directory contents onto manifest file roles.
func classifyFiles(names []string) (map[string]string, string) {
	files := map[string]string{}
	var note string

	pick := func(role string, match func(string) bool) {
		var chosen string
		for _, n := range names {
			if !strings.HasSuffix(n, ".onnx") || !match(n) {
				continue
			}
			// Prefer the quantised weights: they are what a CPU deployment
			// should run, and shipping both in a manifest is ambiguous.
			if chosen == "" || (strings.Contains(n, ".int8.") && !strings.Contains(chosen, ".int8.")) {
				chosen = n
			}
		}
		if chosen != "" {
			files[role] = chosen
		}
	}

	has := func(sub string) func(string) bool {
		return func(n string) bool { return strings.Contains(n, sub) }
	}

	pick("encoder", has("encoder"))
	pick("decoder", has("decoder"))
	pick("joiner", has("joiner"))

	if files["encoder"] == "" || files["joiner"] == "" {
		// Not a transducer: fall back to the single-model shape.
		delete(files, "encoder")
		delete(files, "decoder")
		delete(files, "joiner")
		pick("model", func(n string) bool { return true })
	}

	for _, n := range names {
		switch {
		case n == "tokens.txt":
			files["tokens"] = n
		case n == "bpe.model" || n == "bpe.vocab":
			files["bpe_vocab"] = n
			note = "found " + n + ", wired as files.bpe_vocab"
		}
	}
	return files, note
}

func detectFamily(files map[string]string, meta Metadata) string {
	if files["joiner"] != "" {
		return "transducer"
	}
	switch mt := meta["model_type"]; {
	case strings.Contains(mt, "EncDecCTC"), strings.Contains(mt, "nemo"):
		return "nemo_ctc"
	case strings.Contains(mt, "zipformer"):
		return "zipformer_ctc"
	case strings.Contains(mt, "wenet"):
		return "wenet_ctc"
	case strings.Contains(mt, "telespeech"):
		return "telespeech"
	default:
		return "nemo_ctc"
	}
}

func familySource(files map[string]string, meta Metadata) string {
	if files["joiner"] != "" {
		return "the encoder/decoder/joiner file set"
	}
	if mt := meta["model_type"]; mt != "" {
		return "ONNX model_type " + mt
	}
	return "a single .onnx file, defaulted — confirm against the model card"
}

// detectModelingUnit reads tokens.txt and decides how tokens group into words.
//
// This drives word assembly, and getting it wrong does not fail either: a
// character vocabulary read as BPE produces one word per character.
func detectModelingUnit(path string) (unit, note string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	var total, withMarker, singleRune, cjk int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		tok := strings.Fields(sc.Text())
		if len(tok) == 0 {
			continue
		}
		t := tok[0]
		if strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">") {
			continue // <blk>, <unk>, <sos/eos>
		}
		total++
		if strings.Contains(t, "▁") {
			withMarker++
			t = strings.ReplaceAll(t, "▁", "")
		}
		runes := []rune(t)
		if len(runes) == 1 {
			singleRune++
			if unicode.Is(unicode.Han, runes[0]) || unicode.Is(unicode.Hiragana, runes[0]) ||
				unicode.Is(unicode.Katakana, runes[0]) || unicode.Is(unicode.Hangul, runes[0]) {
				cjk++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	if total == 0 {
		return "", "", fmt.Errorf("tokens.txt has no usable entries")
	}

	switch {
	case withMarker > 0 && cjk*4 > total:
		return "cjkchar+bpe", fmt.Sprintf(
			"modeling_unit cjkchar+bpe: %d/%d tokens carry the SentencePiece marker and %d are CJK",
			withMarker, total, cjk), nil
	case withMarker > 0:
		return "bpe", fmt.Sprintf(
			"modeling_unit bpe: %d/%d tokens carry the SentencePiece marker", withMarker, total), nil
	case cjk*2 > total:
		return "cjkchar", fmt.Sprintf(
			"modeling_unit cjkchar: %d/%d tokens are single CJK characters", cjk, total), nil
	case singleRune*10 > total*9: // at least 90% single characters
		return "char", fmt.Sprintf(
			"modeling_unit char: %d/%d tokens are single characters and none carry a "+
				"SentencePiece marker; the separator is expected to be a literal space",
			singleRune, total), nil
	default:
		return "bpe", fmt.Sprintf(
			"modeling_unit bpe, defaulted: %d/%d tokens are multi-character but none carry "+
				"a marker — confirm against the model card", total-singleRune, total), nil
	}
}

// onnxMetadataKeys are the entries worth surfacing; the rest are noise.
var onnxMetadataKeys = []string{
	"model_type", "vocab_size", "is_giga_am", "language", "license",
	"model_author", "subsampling_factor", "normalize_type", "feat_dim", "feature_dim",
}

// ReadONNXMetadata extracts metadata_props from an ONNX file.
//
// ONNX is protobuf and metadata_props is field 14 of ModelProto, which
// serialises after the graph — so the entries sit at the end of the file and a
// tail read finds them without loading hundreds of megabytes. Each entry is a
// StringStringEntryProto: field 1 is the key, field 2 the value, which makes
// the extraction exact rather than a text search.
func ReadONNXMetadata(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	const tail = 128 << 10
	offset := int64(0)
	size := info.Size()
	if size > tail {
		offset = size - tail
		size = tail
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, err
	}

	return parseMetadata(buf), nil
}

func parseMetadata(buf []byte) Metadata {
	out := Metadata{}
	for _, key := range onnxMetadataKeys {
		if v, ok := findStringEntry(buf, key); ok {
			out[key] = v
		}
	}
	return out
}

// findStringEntry locates `0x0A <len> key 0x12 <len> value` — the wire form of
// a StringStringEntryProto whose key matches.
func findStringEntry(buf []byte, key string) (string, bool) {
	needle := append([]byte{0x0A, byte(len(key))}, key...)

	for from := 0; ; {
		idx := bytes.Index(buf[from:], needle)
		if idx < 0 {
			return "", false
		}
		pos := from + idx + len(needle)
		from = pos

		if pos >= len(buf) || buf[pos] != 0x12 {
			continue // the key string appears somewhere that is not an entry
		}
		length, n := binary.Uvarint(buf[pos+1:])
		if n <= 0 || length == 0 || pos+1+n+int(length) > len(buf) {
			continue
		}
		value := string(buf[pos+1+n : pos+1+n+int(length)])
		if isPrintable(value) {
			return value, true
		}
	}
}

func isPrintable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

// languageCode maps the free-form language names models put in their metadata
// onto the short codes a request carries. Unknown names pass through lowercased
// so nothing is silently dropped.
var languageNames = map[string]string{
	"russian": "ru", "english": "en", "chinese": "zh", "mandarin": "zh",
	"german": "de", "french": "fr", "spanish": "es", "japanese": "ja",
	"korean": "ko", "portuguese": "pt", "italian": "it", "arabic": "ar",
}

func languageCode(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if code, ok := languageNames[lower]; ok {
		return code
	}
	return lower
}

// suggestID derives a plausible model id from the directory name.
func suggestID(dir string) string {
	base := filepath.Base(strings.TrimSuffix(dir, string(filepath.Separator)))
	base = strings.TrimPrefix(base, "sherpa-onnx-")
	if idPattern.MatchString(base) {
		return base
	}
	return "model"
}
