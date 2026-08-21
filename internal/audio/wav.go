package audio

import (
	"context"
	"io"

	"github.com/usunrise88/nanoasr/internal/core"
)

// WAVDecoder handles RIFF/WAVE natively: PCM s16le/s24le/s32le, IEEE float32,
// and the two telephony companding codecs, A-law and mu-law.
//
// This is the path most telephony recordings take, so it must not allocate more
// than one copy of the audio and must never shell out.
type WAVDecoder struct{}

func NewWAVDecoder() *WAVDecoder { return &WAVDecoder{} }

func (*WAVDecoder) CanDecode(f Format) bool { return f == FormatWAV }

// TODO(M1): parse the RIFF chunk list, reject unknown wFormatTag with a domain
// error, stream-convert to float32 and resample with Resample.
func (*WAVDecoder) Decode(context.Context, io.Reader, Options) (PCM, error) {
	return PCM{}, core.ErrNotImplemented
}
