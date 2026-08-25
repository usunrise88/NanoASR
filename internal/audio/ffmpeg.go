package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// FFmpegDecoder shells out for every format the native path does not cover:
// mp3, ogg/opus/vorbis, flac, m4a/aac, amr, gsm, webm and the rest.
//
// Security posture (SPEC §11): the argument vector is fixed in code, the input
// arrives on stdin so no user-controlled string is ever an argument, and the
// process is killed through the context plus Cmd.WaitDelay so a wedged ffmpeg
// cannot outlive the request that started it.
type FFmpegDecoder struct {
	path    string
	timeout time.Duration
}

// NewFFmpegDecoder returns nil when ffmpeg is not configured or not installed,
// so the caller degrades to WAV-only instead of failing to start. A nil
// decoder answers CanDecode false and the router reports unsupported_media_type.
func NewFFmpegDecoder(path string, timeout time.Duration) *FFmpegDecoder {
	if path == "" {
		return nil
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &FFmpegDecoder{path: resolved, timeout: timeout}
}

// Path is the resolved binary, for startup logging.
func (d *FFmpegDecoder) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

func (d *FFmpegDecoder) CanDecode(f Format) bool {
	return d != nil && f != FormatUnknown && !f.Native()
}

// args is the only command line we ever run. Every element is a fixed string
// or a number we produced: nothing the client sent reaches it (SPEC §11).
//
// The three channel modes need three different tails, and conflating any two
// of them is a silent wrong answer rather than an error:
//
//   - downmix averages the channels, which is what -ac 1 does;
//   - first keeps one leg, which is a filter — -ac 1 would average them
//     instead of dropping one, so the two are not interchangeable;
//   - split needs every channel, so it asks for no channel conversion at all
//     and writes a WAV, which our own decoder then deinterleaves. Emitting
//     raw f32le here would leave the channel count nowhere in the stream.
func (d *FFmpegDecoder) args(opts Options) []string {
	rate := opts.TargetSampleRate
	if rate <= 0 {
		rate = 16000
	}
	const resample = "aresample=resampler=soxr"

	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-sn", "-dn",
		"-map", "0:a:0",
	}
	switch opts.ChannelMode {
	case core.ChannelSplit:
		args = append(args,
			"-ar", strconv.Itoa(rate),
			"-af", resample,
			"-c:a", "pcm_f32le",
			"-f", "wav")
	case core.ChannelFirst:
		args = append(args,
			"-ar", strconv.Itoa(rate),
			"-af", "pan=mono|c0=c0,"+resample,
			"-f", "f32le")
	default:
		args = append(args,
			"-ac", "1",
			"-ar", strconv.Itoa(rate),
			"-af", resample,
			"-f", "f32le")
	}
	return append(args, "pipe:1")
}

func (d *FFmpegDecoder) Decode(ctx context.Context, r io.Reader, opts Options) ([]PCM, error) {
	if d == nil {
		return nil, core.Errorf(core.CodeUnsupportedMediaType, "ffmpeg is not configured")
	}

	rate := opts.TargetSampleRate
	if rate <= 0 {
		rate = 16000
	}

	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.path, d.args(opts)...)
	// Cancel asks politely; WaitDelay is what guarantees the process is gone
	// even if it ignores the signal or a child keeps the pipe open.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 5 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, internalErr(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, internalErr(err)
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, internalErr(err)
	}

	// Feed the input on a goroutine: ffmpeg rejecting a malformed file closes
	// stdin early, and a synchronous copy would deadlock against our own read
	// of stdout.
	var feed sync.WaitGroup
	feed.Add(1)
	go func() {
		defer feed.Done()
		defer stdin.Close()
		_, _ = io.Copy(stdin, r)
	}()

	out, readErr := d.consume(ctx, stdout, opts, rate)

	// Drain whatever is left so ffmpeg is never blocked writing into a full
	// pipe while we wait for it to exit.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	feed.Wait()

	switch {
	case readErr != nil:
		return nil, readErr
	case waitErr != nil:
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, core.Errorf(core.CodeProcessingTimeout,
				"ffmpeg exceeded the %s decode timeout", d.timeout)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// ffmpeg's own diagnosis is far more useful than "exit status 1", but
		// it goes to the log, not to the client.
		return nil, core.Errorf(core.CodeUnsupportedMediaType,
			"ffmpeg could not decode this input").
			WithCause(errors.New(stderr.String()))
	case len(out) == 0 || len(out[0].Samples) == 0:
		return nil, core.Errorf(core.CodeInvalidRequest,
			"input contains no decodable audio stream")
	}

	return out, nil
}

// consume turns ffmpeg's stdout into PCM. Under split that stdout is a WAV
// stream, so it goes through the native decoder rather than through a second
// deinterleaver written here: one implementation of "interleaved frames to
// mono tracks" is enough, and it is already the tested one.
func (d *FFmpegDecoder) consume(ctx context.Context, stdout io.Reader, opts Options, rate int) ([]PCM, error) {
	if opts.ChannelMode == core.ChannelSplit {
		return NewWAVDecoder().Decode(ctx, stdout, opts)
	}
	samples, err := readFloat32LE(ctx, stdout, rate, opts.MaxDurationSec, opts.MaxDecodedBytes)
	if err != nil {
		return nil, err
	}
	// ffmpeg has already mixed to one channel, so the source count it started
	// from is not observable from here.
	return []PCM{{Samples: samples, SampleRate: rate, SourceChannels: 1}}, nil
}

// readFloat32LE streams ffmpeg's raw output, stopping as soon as the duration
// limit is crossed rather than after the whole file has been decoded.
func readFloat32LE(ctx context.Context, r io.Reader, rate int, maxDurationSec float64, maxBytes int64) ([]float32, error) {
	maxSamples := -1
	if maxDurationSec > 0 {
		maxSamples = int(maxDurationSec * float64(rate))
	}
	// One mono track here, so the memory cap is a second ceiling on the same
	// stream rather than a per-channel one.
	if maxBytes > 0 {
		if byBytes := int(maxBytes / 4); maxSamples < 0 || byBytes < maxSamples {
			maxSamples = byBytes
		}
	}

	const blockSamples = 8192
	buf := make([]byte, blockSamples*4)
	var out []float32
	var carry [4]byte
	carried := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		n, err := r.Read(buf)
		if n > 0 {
			data := buf[:n]
			// A read can split a sample; keep the remainder for the next one.
			if carried > 0 {
				need := 4 - carried
				if len(data) < need {
					copy(carry[carried:], data)
					carried += len(data)
					continue
				}
				copy(carry[carried:], data[:need])
				out = append(out, float32frombits(binary.LittleEndian.Uint32(carry[:])))
				data = data[need:]
				carried = 0
			}

			whole := len(data) - len(data)%4
			for i := 0; i < whole; i += 4 {
				out = append(out, float32frombits(binary.LittleEndian.Uint32(data[i:i+4])))
			}
			if rest := data[whole:]; len(rest) > 0 {
				carried = copy(carry[:], rest)
			}

			if maxSamples >= 0 && len(out) > maxSamples {
				return nil, core.Errorf(core.CodeDurationExceeded,
					"audio exceeds the %.1fs limit", maxDurationSec)
			}
		}
		if err != nil {
			if isEOF(err) {
				return out, nil
			}
			return nil, internalErr(err)
		}
	}
}

// tailBuffer keeps only the last few kilobytes written to it. ffmpeg on a
// broken file can emit a great deal of stderr, and none of it is worth
// unbounded memory.
type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

const tailBufferLimit = 8 << 10

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > tailBufferLimit {
		b.data = b.data[len(b.data)-tailBufferLimit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}

func internalErr(err error) error {
	return core.Errorf(core.CodeInternal, "audio decode failed").WithCause(err)
}
