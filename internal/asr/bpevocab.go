package asr

import (
	"encoding/binary"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/usunrise88/nanoasr/internal/core"
)

// BPEVocabulary renders a model's subword vocabulary in the one form
// sherpa-onnx can read: one line per piece, "<piece> <score>".
//
// It exists because the file that ships with a model is usually the other one.
// SentencePiece writes a binary protobuf, conventionally called bpe.model, and
// that is what sherpa-onnx's own model archives carry — including the one this
// catalog points at for streaming-zipformer-small-ru. Handed that file,
// sherpa-onnx does not report a format error:
//
//	Each line in vocab should contain two items (seperate by space), the first
//	one is bpe token, the second one is score, given :
//
// and calls exit(). Measured, and it takes the server with it mid-request.
//
// So the format is decided here, where a bad file is an error a caller can be
// told about. converted says the text had to be built, which is the caller's
// cue that it has nothing on disk to point sherpa-onnx at.
func BPEVocabulary(path string) (text string, converted bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, core.Errorf(core.CodeModelUnavailable,
			"cannot read the subword vocabulary %s", path).WithCause(err)
	}
	if isTextVocabulary(data) {
		return string(data), false, nil
	}
	pieces, ok := sentencePieces(data)
	if !ok || len(pieces) == 0 {
		return "", false, core.Errorf(core.CodeModelUnavailable,
			"%s is neither a sentencepiece model nor a \"<piece> <score>\" vocabulary; "+
				"sherpa-onnx reads the second form, and bpe.model from a model archive is the first",
			path)
	}

	var b strings.Builder
	for _, p := range pieces {
		b.WriteString(p.piece)
		b.WriteByte(' ')
		// The shortest decimal that round-trips the float32 the proto carried:
		// the score has no more precision than that to begin with.
		b.WriteString(strconv.FormatFloat(float64(p.score), 'g', -1, 32))
		b.WriteByte('\n')
	}
	return b.String(), true, nil
}

// isTextVocabulary reports whether the bytes already are what sherpa-onnx
// wants. The first non-empty line decides: a piece, a space, and a number.
//
// A sentencepiece model fails this on its first line — the bytes after the
// piece are a length prefix and a little-endian float, not a decimal — so the
// two formats are told apart by reading them rather than by their extension,
// which in practice is bpe.model for both.
func isTextVocabulary(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i <= 0 {
			return false
		}
		_, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		return err == nil
	}
	return false
}

type sentencePiece struct {
	piece string
	score float32
}

// sentencePieces reads the pieces out of a SentencePiece ModelProto.
//
// Hand-written rather than generated: the whole of what is needed is the
// repeated field 1 and, inside it, a string and a float32. Pulling in the
// protobuf runtime and the sentencepiece schema to read two fields would be a
// dependency the rest of this binary has no use for.
//
//	message ModelProto {
//	  message SentencePiece { string piece = 1; float score = 2; Type type = 3; }
//	  repeated SentencePiece pieces = 1;
//	  ...
//	}
//
// Returns ok=false for anything that is not a well-formed message, which is how
// a file in neither format is told apart from a vocabulary.
func sentencePieces(data []byte) ([]sentencePiece, bool) {
	var out []sentencePiece
	for len(data) > 0 {
		field, wire, rest, ok := protoKey(data)
		if !ok {
			return nil, false
		}
		data = rest
		switch wire {
		case 2: // length-delimited
			n, rest, ok := protoVarint(data)
			if !ok || n > uint64(len(rest)) {
				return nil, false
			}
			body, remainder := rest[:n], rest[n:]
			data = remainder
			if field != 1 {
				continue // trainer_spec and the rest of the model, not needed
			}
			p, ok := onePiece(body)
			if !ok {
				return nil, false
			}
			out = append(out, p)
		case 0:
			_, rest, ok := protoVarint(data)
			if !ok {
				return nil, false
			}
			data = rest
		case 5:
			if len(data) < 4 {
				return nil, false
			}
			data = data[4:]
		case 1:
			if len(data) < 8 {
				return nil, false
			}
			data = data[8:]
		default:
			return nil, false
		}
	}
	return out, true
}

func onePiece(body []byte) (sentencePiece, bool) {
	var p sentencePiece
	for len(body) > 0 {
		field, wire, rest, ok := protoKey(body)
		if !ok {
			return p, false
		}
		body = rest
		switch {
		case wire == 2:
			n, rest, ok := protoVarint(body)
			if !ok || n > uint64(len(rest)) {
				return p, false
			}
			if field == 1 {
				p.piece = string(rest[:n])
			}
			body = rest[n:]
		case wire == 5:
			if len(body) < 4 {
				return p, false
			}
			if field == 2 {
				p.score = math.Float32frombits(binary.LittleEndian.Uint32(body[:4]))
			}
			body = body[4:]
		case wire == 0:
			_, rest, ok := protoVarint(body)
			if !ok {
				return p, false
			}
			body = rest
		case wire == 1:
			if len(body) < 8 {
				return p, false
			}
			body = body[8:]
		default:
			return p, false
		}
	}
	// A piece with no text is not a piece. Reporting it as a parse failure is
	// what keeps a file that merely happens to start with 0x0a from being read
	// as a vocabulary of empty strings.
	return p, p.piece != ""
}

func protoKey(b []byte) (field int, wire int, rest []byte, ok bool) {
	v, rest, ok := protoVarint(b)
	if !ok {
		return 0, 0, nil, false
	}
	return int(v >> 3), int(v & 7), rest, true
}

func protoVarint(b []byte) (uint64, []byte, bool) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, b[i+1:], true
		}
	}
	return 0, nil, false
}
