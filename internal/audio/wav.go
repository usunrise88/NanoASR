package audio

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/usunrise88/nanoasr/internal/core"
)

// WAV format tags we accept. Anything else is refused by name so the operator
// learns what arrived rather than seeing generic noise.
const (
	wavPCM        = 0x0001
	wavIEEEFloat  = 0x0003
	wavALaw       = 0x0006
	wavMuLaw      = 0x0007
	wavExtensible = 0xFFFE
)

// maxFmtChunk bounds the one chunk whose body we materialise. Real fmt chunks
// are 16, 18 or 40 bytes; the length field is attacker-controlled and would
// otherwise turn a 44-byte file into a 4 GB allocation.
const maxFmtChunk = 4096

// maxPrealloc caps the sample buffer we reserve up front, about 4.4 minutes at
// 16 kHz. A declared data size is a hint, not a promise: append grows the slice
// for genuinely longer audio, and a lying header costs nothing.
const maxPrealloc = 1 << 22

// WAVDecoder handles RIFF/WAVE natively: PCM 8/16/24/32, IEEE float32, and the
// two telephony companding codecs, A-law and mu-law.
//
// This is the path most telephony recordings take, so it does not shell out and
// it converts in place rather than materialising an intermediate copy.
type WAVDecoder struct{}

func NewWAVDecoder() *WAVDecoder { return &WAVDecoder{} }

func (*WAVDecoder) CanDecode(f Format) bool { return f == FormatWAV }

type wavFormat struct {
	tag           int
	channels      int
	sampleRate    int
	bitsPerSample int
	blockAlign    int
}

func (d *WAVDecoder) Decode(ctx context.Context, r io.Reader, opts Options) (PCM, error) {
	br := bufio.NewReaderSize(r, 64<<10)

	f, dataSize, err := readWAVHeader(br)
	if err != nil {
		return PCM{}, err
	}

	target := opts.TargetSampleRate
	if target <= 0 {
		target = 16000
	}

	samples, err := d.readSamples(ctx, br, f, dataSize, opts)
	if err != nil {
		return PCM{}, err
	}

	if f.sampleRate != target {
		samples = Resample(samples, f.sampleRate, target)
	}
	return PCM{Samples: samples, SampleRate: target, Channels: f.channels}, nil
}

// readWAVHeader walks the chunk list to the data chunk. Chunks other than
// "fmt " and "data" are skipped, which is what lets us read files carrying LIST
// metadata or a fact chunk without special-casing each one.
//
// dataSize is 0 when the header does not state a usable length, which happens
// with streamed WAV; the caller then reads to EOF.
func readWAVHeader(r *bufio.Reader) (wavFormat, int64, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return wavFormat{}, 0, badWAV("truncated RIFF header: %v", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return wavFormat{}, 0, badWAV("not a RIFF/WAVE file")
	}

	var f wavFormat
	haveFmt := false

	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return wavFormat{}, 0, badWAV("no data chunk found")
			}
			return wavFormat{}, 0, badWAV("reading chunk header: %v", err)
		}
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))

		switch id {
		case "fmt ":
			if size < 16 || size > maxFmtChunk {
				return wavFormat{}, 0, badWAV("fmt chunk is %d bytes, expected between 16 and %d",
					size, maxFmtChunk)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				return wavFormat{}, 0, badWAV("truncated fmt chunk: %v", err)
			}
			var err error
			if f, err = parseFmt(body); err != nil {
				return wavFormat{}, 0, err
			}
			haveFmt = true

		case "data":
			if !haveFmt {
				return wavFormat{}, 0, badWAV("data chunk precedes fmt chunk")
			}
			// 0 and 0xFFFFFFFF both mean "length unknown" in streamed files.
			if size == 0 || size == 0xFFFFFFFF {
				size = 0
			}
			return f, size, nil

		default:
			if _, err := r.Discard(int(size)); err != nil {
				return wavFormat{}, 0, badWAV("skipping %q chunk: %v", id, err)
			}
		}

		// Chunks are word-aligned: an odd size carries a pad byte.
		if size%2 == 1 {
			if _, err := r.Discard(1); err != nil && !errors.Is(err, io.EOF) {
				return wavFormat{}, 0, badWAV("skipping pad byte: %v", err)
			}
		}
	}
}

func parseFmt(b []byte) (wavFormat, error) {
	f := wavFormat{
		tag:           int(binary.LittleEndian.Uint16(b[0:2])),
		channels:      int(binary.LittleEndian.Uint16(b[2:4])),
		sampleRate:    int(binary.LittleEndian.Uint32(b[4:8])),
		blockAlign:    int(binary.LittleEndian.Uint16(b[12:14])),
		bitsPerSample: int(binary.LittleEndian.Uint16(b[14:16])),
	}

	// WAVE_FORMAT_EXTENSIBLE hides the real codec in the first two bytes of the
	// SubFormat GUID.
	if f.tag == wavExtensible {
		if len(b) < 40 {
			return f, badWAV("extensible fmt chunk is %d bytes, need 40", len(b))
		}
		f.tag = int(binary.LittleEndian.Uint16(b[24:26]))
	}

	switch f.tag {
	case wavPCM, wavIEEEFloat, wavALaw, wavMuLaw:
	default:
		return f, core.Errorf(core.CodeUnsupportedMediaType,
			"WAV format tag 0x%04X is not supported natively; install ffmpeg to decode it", f.tag)
	}

	if f.channels < 1 || f.channels > 64 {
		return f, badWAV("channel count %d is out of range", f.channels)
	}
	if f.sampleRate < 1000 || f.sampleRate > 384000 {
		return f, badWAV("sample rate %d Hz is out of range", f.sampleRate)
	}

	bytesPerSample, err := sampleWidth(f)
	if err != nil {
		return f, err
	}
	// Trust the computed stride over a declared blockAlign of 0, which some
	// encoders emit.
	if f.blockAlign <= 0 {
		f.blockAlign = bytesPerSample * f.channels
	}
	if f.blockAlign != bytesPerSample*f.channels {
		return f, badWAV("block align %d does not match %d channels of %d bytes",
			f.blockAlign, f.channels, bytesPerSample)
	}
	return f, nil
}

func sampleWidth(f wavFormat) (int, error) {
	switch f.tag {
	case wavALaw, wavMuLaw:
		if f.bitsPerSample != 8 {
			return 0, badWAV("companded audio must be 8-bit, got %d", f.bitsPerSample)
		}
		return 1, nil
	case wavIEEEFloat:
		if f.bitsPerSample != 32 {
			return 0, badWAV("only 32-bit IEEE float is supported, got %d", f.bitsPerSample)
		}
		return 4, nil
	default: // wavPCM
		switch f.bitsPerSample {
		case 8, 16, 24, 32:
			return f.bitsPerSample / 8, nil
		default:
			return 0, badWAV("PCM bit depth %d is not supported", f.bitsPerSample)
		}
	}
}

// readSamples converts frames to mono float32 as they arrive.
//
// The duration limit is enforced while reading, not after: buffering a
// two-hour file only to reject it is how a size limit becomes a memory
// exhaustion bug.
func (d *WAVDecoder) readSamples(ctx context.Context, r io.Reader, f wavFormat, dataSize int64, opts Options) ([]float32, error) {
	width, err := sampleWidth(f)
	if err != nil {
		return nil, err
	}

	maxFrames := int64(-1)
	if opts.MaxDurationSec > 0 {
		maxFrames = int64(opts.MaxDurationSec * float64(f.sampleRate))
	}

	if dataSize > 0 {
		if maxFrames >= 0 && dataSize/int64(f.blockAlign) > maxFrames {
			return nil, core.Errorf(core.CodeDurationExceeded,
				"audio is %.1fs, limit is %.1fs",
				float64(dataSize/int64(f.blockAlign))/float64(f.sampleRate), opts.MaxDurationSec)
		}
		r = io.LimitReader(r, dataSize)
	}

	// One frame per iteration would dominate the profile on a ten-minute file;
	// read whole blocks and convert them.
	const blockFrames = 8192
	buf := make([]byte, blockFrames*f.blockAlign)

	out := make([]float32, 0, preallocFrames(dataSize, int64(f.blockAlign), maxFrames))

	first := opts.ChannelMode == core.ChannelFirst
	frames := int64(0)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		n, err := io.ReadFull(r, buf)
		if n == 0 {
			if isEOF(err) {
				break
			}
			return nil, badWAV("reading samples: %v", err)
		}
		// A partial final read is normal; drop any trailing incomplete frame.
		n -= n % f.blockAlign

		for off := 0; off < n; off += f.blockAlign {
			out = append(out, frameToMono(buf[off:off+f.blockAlign], f, width, first))
		}
		frames += int64(n / f.blockAlign)

		if maxFrames >= 0 && frames > maxFrames {
			return nil, core.Errorf(core.CodeDurationExceeded,
				"audio exceeds the %.1fs limit", opts.MaxDurationSec)
		}
		if isEOF(err) {
			break
		}
		if err != nil {
			return nil, badWAV("reading samples: %v", err)
		}
	}

	if len(out) == 0 {
		return nil, badWAV("data chunk is empty")
	}
	return out, nil
}

// preallocFrames decides how much to reserve, trusting neither the declared
// data size nor the absence of a duration limit.
func preallocFrames(dataSize, blockAlign, maxFrames int64) int {
	if dataSize <= 0 || blockAlign <= 0 {
		return 0
	}
	frames := dataSize / blockAlign
	if maxFrames >= 0 && frames > maxFrames {
		frames = maxFrames
	}
	if frames > maxPrealloc {
		frames = maxPrealloc
	}
	return int(frames)
}

// frameToMono converts one frame, either by averaging channels or by keeping
// the first. Averaging is the default because a downmixed conversation loses
// less than an arbitrarily chosen leg.
func frameToMono(frame []byte, f wavFormat, width int, firstOnly bool) float32 {
	if firstOnly || f.channels == 1 {
		return sampleToFloat(frame[:width], f)
	}
	var sum float32
	for c := 0; c < f.channels; c++ {
		sum += sampleToFloat(frame[c*width:(c+1)*width], f)
	}
	return sum / float32(f.channels)
}

func sampleToFloat(b []byte, f wavFormat) float32 {
	switch f.tag {
	case wavALaw:
		return float32(alawTable[b[0]]) / 32768
	case wavMuLaw:
		return float32(ulawTable[b[0]]) / 32768
	case wavIEEEFloat:
		return float32frombits(binary.LittleEndian.Uint32(b))
	}

	switch f.bitsPerSample {
	case 8:
		// 8-bit PCM in WAV is unsigned, centred on 128.
		return (float32(b[0]) - 128) / 128
	case 16:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case 24:
		v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
		if v&0x800000 != 0 {
			v |= ^0xFFFFFF // sign-extend the 24-bit value
		}
		return float32(v) / 8388608
	default: // 32
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	}
}

func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func badWAV(format string, args ...any) error {
	return core.Errorf(core.CodeInvalidRequest, "invalid WAV: %s", fmt.Sprintf(format, args...))
}
