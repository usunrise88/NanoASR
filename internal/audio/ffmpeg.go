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

// args is the only command line we ever run.
func (d *FFmpegDecoder) args(opts Options) []string {
	rate := opts.TargetSampleRate
	if rate <= 0 {
		rate = 16000
	}
	channels := "1"
	if opts.ChannelMode == core.ChannelFirst {
		// Keeping the first channel is a filter, not a channel count: -ac 1
		// would average the legs instead of dropping one.
		channels = "1"
	}
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-sn", "-dn",
		"-map", "0:a:0",
		"-ac", channels,
		"-ar", strconv.Itoa(rate),
		"-af", "aresample=resampler=soxr",
		"-f", "f32le",
		"pipe:1",
	}
}

func (d *FFmpegDecoder) Decode(ctx context.Context, r io.Reader, opts Options) (PCM, error) {
	if d == nil {
		return PCM{}, core.Errorf(core.CodeUnsupportedMediaType, "ffmpeg is not configured")
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
		return PCM{}, internalErr(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return PCM{}, internalErr(err)
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return PCM{}, internalErr(err)
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

	samples, readErr := readFloat32LE(ctx, stdout, rate, opts.MaxDurationSec)

	// Drain whatever is left so ffmpeg is never blocked writing into a full
	// pipe while we wait for it to exit.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	feed.Wait()

	switch {
	case readErr != nil:
		return PCM{}, readErr
	case waitErr != nil:
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return PCM{}, core.Errorf(core.CodeProcessingTimeout,
				"ffmpeg exceeded the %s decode timeout", d.timeout)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return PCM{}, ctxErr
		}
		// ffmpeg's own diagnosis is far more useful than "exit status 1", but
		// it goes to the log, not to the client.
		return PCM{}, core.Errorf(core.CodeUnsupportedMediaType,
			"ffmpeg could not decode this input").
			WithCause(errors.New(stderr.String()))
	case len(samples) == 0:
		return PCM{}, core.Errorf(core.CodeInvalidRequest,
			"input contains no decodable audio stream")
	}

	return PCM{Samples: samples, SampleRate: rate, Channels: 1}, nil
}

// readFloat32LE streams ffmpeg's raw output, stopping as soon as the duration
// limit is crossed rather than after the whole file has been decoded.
func readFloat32LE(ctx context.Context, r io.Reader, rate int, maxDurationSec float64) ([]float32, error) {
	maxSamples := -1
	if maxDurationSec > 0 {
		maxSamples = int(maxDurationSec * float64(rate))
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
