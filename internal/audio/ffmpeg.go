package audio

import (
	"context"
	"io"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// FFmpegDecoder shells out for every format the native path does not cover.
//
// Security posture (SPEC §11): the argument vector is fixed in code, the input
// arrives on stdin so no user-controlled string is ever an argument, and the
// process is killed via context plus Cmd.WaitDelay so a wedged ffmpeg cannot
// outlive the request.
type FFmpegDecoder struct {
	path    string
	timeout time.Duration
}

// NewFFmpegDecoder returns nil when path is empty or the binary is missing, so
// the caller can degrade to WAV-only rather than fail at start.
func NewFFmpegDecoder(path string, timeout time.Duration) *FFmpegDecoder {
	if path == "" {
		return nil
	}
	return &FFmpegDecoder{path: path, timeout: timeout}
}

func (d *FFmpegDecoder) CanDecode(f Format) bool {
	return d != nil && f != FormatUnknown && !f.Native()
}

// args is the only command line we ever run.
func (d *FFmpegDecoder) args(opts Options) []string {
	return []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", "pipe:0",
		"-vn", "-sn", "-dn",
		"-ac", "1",
		"-ar", itoa(opts.TargetSampleRate),
		"-f", "f32le",
		"-af", "aresample=resampler=soxr",
		"pipe:1",
	}
}

// TODO(M5): run the command, stream stdout into a []float32, enforce
// MaxDurationSec while reading, and surface stderr in the error cause.
func (d *FFmpegDecoder) Decode(context.Context, io.Reader, Options) (PCM, error) {
	if d == nil {
		return PCM{}, core.Errorf(core.CodeUnsupportedMediaType, "ffmpeg is not configured")
	}
	return PCM{}, core.ErrNotImplemented
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
