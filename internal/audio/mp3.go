package audio

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// MP3Bitrate is what the isolated voice is encoded at: one 16 kHz mono voice,
// for which 64 kbit/s is transparent and a quarter the size of the WAV.
const MP3Bitrate = "64k"

// mp3Args is the encoder's fixed command line, under the same rule as the
// decoder's: nothing a client sent is an argument (SPEC §11). The input is the
// WAV the server produced, on stdin.
var mp3Args = []string{
	"-hide_banner", "-nostdin", "-loglevel", "error",
	"-f", "wav", "-i", "pipe:0",
	"-vn", "-c:a", "libmp3lame", "-b:a", MP3Bitrate,
	"-f", "mp3", "pipe:1",
}

// EncodeMP3 streams in, a WAV, through ffmpeg into MP3. The returned reader
// yields the MP3 as ffmpeg writes it; closing it stops the encoder, and so
// does ctx. An encoder failure surfaces as the error of a Read.
func (d *FFmpegDecoder) EncodeMP3(ctx context.Context, in io.ReadCloser) (io.ReadCloser, error) {
	if d == nil {
		_ = in.Close()
		return nil, core.Errorf(core.CodeCapabilityUnavailable, "MP3 needs ffmpeg, which is not installed")
	}
	ctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(ctx, d.path, mp3Args...)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = in
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = in.Close()
		return nil, internalErr(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = in.Close()
		return nil, internalErr(err)
	}
	return &encoderOutput{r: stdout, cmd: cmd, in: in, cancel: cancel, stderr: &stderr}, nil
}

type encoderOutput struct {
	r      io.Reader
	cmd    *exec.Cmd
	in     io.Closer
	cancel context.CancelFunc
	stderr *tailBuffer

	once    sync.Once
	waitErr error
}

func (e *encoderOutput) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		// The end of the stream is only a success if ffmpeg says so.
		if werr := e.wait(); werr != nil {
			return n, core.Errorf(core.CodeInternal, "ffmpeg could not encode MP3").
				WithCause(errors.New(e.stderr.String()))
		}
	}
	return n, err
}

func (e *encoderOutput) wait() error {
	e.once.Do(func() { e.waitErr = e.cmd.Wait() })
	return e.waitErr
}

func (e *encoderOutput) Close() error {
	e.cancel()
	_ = e.wait()
	return e.in.Close()
}
