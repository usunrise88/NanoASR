// Package audio turns whatever the client uploaded into the one representation
// the rest of NanoASR understands: mono float32 at 16 kHz.
//
// Two decode paths exist by design (SPEC §5.2): a native Go path for WAV/PCM,
// which is the common telephony case and costs no process, and an ffmpeg
// subprocess for everything else. ffmpeg is optional: without it, non-WAV input
// fails with unsupported_media_type instead of taking the server down with it.
package audio

import (
	"bytes"
	"context"
	"io"

	"github.com/usunrise88/nanoasr/internal/core"
)

// PCM is canonical audio: mono, float32 in [-1,1], SampleRate Hz.
//
// One PCM is always one mono signal. Under ChannelSplit a decoder returns
// several of them, one per source channel, rather than widening this type:
// every stage downstream — VAD, the recogniser, word assembly — takes mono, and
// a PCM that were sometimes interleaved would put a "which kind is this?"
// branch in all of them.
type PCM struct {
	Samples    []float32
	SampleRate int
	// SourceChannels is how many channels the file had before any downmix,
	// kept for reporting. Channel says which one this PCM carries, and is 0
	// both for channel 0 of a split and for a downmix of all of them —
	// the two are told apart by how many PCMs came back.
	SourceChannels int
	Channel        int
}

// Duration in seconds.
func (p PCM) Duration() float64 {
	if p.SampleRate == 0 {
		return 0
	}
	return float64(len(p.Samples)) / float64(p.SampleRate)
}

// Format is a container/codec family, detected from magic bytes only — never
// from the file extension or the client-supplied Content-Type.
type Format string

const (
	FormatUnknown  Format = "unknown"
	FormatWAV      Format = "wav"
	FormatMP3      Format = "mp3"
	FormatOgg      Format = "ogg" // vorbis or opus
	FormatFLAC     Format = "flac"
	FormatMP4      Format = "mp4" // m4a / aac in mp4
	FormatAMR      Format = "amr"
	FormatMatroska Format = "mkv" // webm
	FormatCAF      Format = "caf"
	FormatAU       Format = "au"
)

// Native reports whether this format is handled without ffmpeg.
func (f Format) Native() bool { return f == FormatWAV }

// Sniff identifies the container from a prefix of the stream. 16 bytes are
// enough for every format we claim to support.
func Sniff(head []byte) Format {
	switch {
	case len(head) >= 12 && bytes.Equal(head[0:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WAVE")):
		return FormatWAV
	case len(head) >= 4 && bytes.Equal(head[0:4], []byte("fLaC")):
		return FormatFLAC
	case len(head) >= 4 && bytes.Equal(head[0:4], []byte("OggS")):
		return FormatOgg
	case len(head) >= 3 && bytes.Equal(head[0:3], []byte("ID3")):
		return FormatMP3
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return FormatMP3 // MPEG audio frame sync
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		return FormatMP4
	case len(head) >= 6 && bytes.Equal(head[0:6], []byte("#!AMR\n")):
		return FormatAMR
	case len(head) >= 4 && bytes.Equal(head[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return FormatMatroska
	case len(head) >= 4 && bytes.Equal(head[0:4], []byte("caff")):
		return FormatCAF
	case len(head) >= 4 && bytes.Equal(head[0:4], []byte(".snd")):
		return FormatAU
	default:
		return FormatUnknown
	}
}

// SniffSize is how many bytes Sniff needs.
const SniffSize = 16

// Decoder produces canonical PCM from an encoded stream.
type Decoder interface {
	// CanDecode is consulted before Decode so the router can pick the cheap
	// path when it applies.
	CanDecode(f Format) bool
	// Decode must respect ctx: a cancelled request has to stop the work, not
	// just stop waiting for it.
	//
	// It returns one PCM per recognised channel: several under ChannelSplit,
	// exactly one otherwise. The slice is never empty on a nil error.
	Decode(ctx context.Context, r io.Reader, opts Options) ([]PCM, error)
}

// Options controls channel handling and the target rate.
type Options struct {
	TargetSampleRate int
	ChannelMode      core.ChannelMode
	// MaxDurationSec aborts decoding of an over-long file instead of buffering
	// it first and rejecting it afterwards.
	MaxDurationSec float64
	// MaxSplitChannels caps how many channels ChannelSplit will decode. A file
	// with more is refused: split exists for a telephony leg pair, and a
	// sixteen-track session would be decoded in full before anyone noticed.
	// 0 means no split-specific limit.
	MaxSplitChannels int
	// MaxDecodedBytes caps decoded PCM across every channel together. The
	// duration limit stopped being sufficient once one file could produce N
	// channels of it. 0 means unlimited.
	MaxDecodedBytes int64
}

// splitChannels reports how many channels to produce for a source that has n.
func (o Options) splitChannels(n int) (int, error) {
	if o.ChannelMode != core.ChannelSplit || n <= 1 {
		return 1, nil
	}
	if o.MaxSplitChannels > 0 && n > o.MaxSplitChannels {
		return 0, core.Errorf(core.CodeInvalidRequest,
			"channel_mode=split accepts at most %d channels, this file has %d",
			o.MaxSplitChannels, n).WithParam("channel_mode")
	}
	return n, nil
}

// checkDecodedBytes refuses a decode that would hold more PCM than allowed.
// frames is per channel; float32 is four bytes.
func (o Options) checkDecodedBytes(frames int64, channels int) error {
	if o.MaxDecodedBytes <= 0 {
		return nil
	}
	if got := frames * int64(channels) * 4; got > o.MaxDecodedBytes {
		return core.Errorf(core.CodeFileTooLarge,
			"decoding this file would need %d bytes of audio memory, the limit is %d",
			got, o.MaxDecodedBytes)
	}
	return nil
}

// Router picks a decoder by format and reports a clean domain error when no
// decoder can take it.
type Router struct {
	decoders []Decoder
}

func NewRouter(decoders ...Decoder) *Router { return &Router{decoders: decoders} }

// Decode sniffs the head of r and dispatches. The sniffed prefix is pushed back
// so decoders always see a complete stream.
func (rt *Router) Decode(ctx context.Context, r io.Reader, opts Options) ([]PCM, Format, error) {
	head := make([]byte, SniffSize)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, FormatUnknown, core.Errorf(core.CodeInvalidRequest, "cannot read audio: %v", err)
	}
	head = head[:n]
	f := Sniff(head)
	full := io.MultiReader(bytes.NewReader(head), r)

	for _, d := range rt.decoders {
		if d.CanDecode(f) {
			pcm, err := d.Decode(ctx, full, opts)
			return pcm, f, err
		}
	}

	// Two different failures wear the same status code, and the difference
	// matters to whoever has to fix it: an unrecognised container is the
	// caller's problem, a missing ffmpeg is the operator's.
	if f == FormatUnknown {
		return nil, f, core.Errorf(core.CodeUnsupportedMediaType,
			"the upload is not recognisable audio")
	}
	if !rt.handlesCompressed() {
		return nil, f, core.Errorf(core.CodeUnsupportedMediaType,
			"format %q needs ffmpeg, which is not installed on the server", f)
	}
	return nil, f, core.Errorf(core.CodeUnsupportedMediaType,
		"no decoder for format %q", f)
}

// handlesCompressed reports whether any registered decoder covers formats the
// native path does not.
func (rt *Router) handlesCompressed() bool {
	for _, d := range rt.decoders {
		if d.CanDecode(FormatMP3) {
			return true
		}
	}
	return false
}
