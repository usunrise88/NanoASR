package audio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

// fixtureAmplitude is what ffmpeg's lavfi sine source actually produces: it
// defaults to a deliberately quiet -18 dBFS tone, not full scale. Measured
// rather than assumed, because assuming full scale makes every codec look lossy.
const fixtureAmplitude = 0.125

// encodeWith produces a real encoded file by asking ffmpeg for one. Hand-rolled
// fixtures would test our idea of an mp3 rather than an actual mp3.
func encodeWith(t *testing.T, args ...string) []byte {
	t.Helper()
	requireFFmpeg(t)

	base := []string{"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1:sample_rate=16000"}
	cmd := exec.Command("ffmpeg", append(base, append(args, "pipe:1")...)...)

	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("encoding fixture: %v\n%s", err, stderr.String())
	}
	return out.Bytes()
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed; the decoder is optional by design")
	}
}

func newTestFFmpeg(t *testing.T) *FFmpegDecoder {
	t.Helper()
	requireFFmpeg(t)
	d := NewFFmpegDecoder("ffmpeg", 30*time.Second)
	if d == nil {
		t.Fatal("NewFFmpegDecoder returned nil although ffmpeg is on PATH")
	}
	return d
}

func TestFFmpegDecodesCompressedFormats(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"mp3", []string{"-f", "mp3"}},
		{"opus", []string{"-c:a", "libopus", "-f", "ogg"}},
		{"flac", []string{"-f", "flac"}},
		{"m4a", []string{"-c:a", "aac", "-f", "adts"}},
		{"alaw-8k", []string{"-ar", "8000", "-c:a", "pcm_alaw", "-f", "wav"}},
	}

	d := newTestFFmpeg(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := encodeWith(t, c.args...)

			pcm, err := d.Decode(context.Background(), bytes.NewReader(raw),
				Options{TargetSampleRate: 16000, MaxDurationSec: 60})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			if pcm.SampleRate != 16000 {
				t.Errorf("SampleRate = %d, want 16000", pcm.SampleRate)
			}
			// A one-second fixture, allowing for codec priming and padding.
			if dur := pcm.Duration(); dur < 0.9 || dur > 1.3 {
				t.Errorf("Duration = %.3fs, want about 1s", dur)
			}
			// The tone must survive the round trip, which lossy codecs and the
			// resampler both get a chance to ruin. A third of the source
			// amplitude is a generous floor that still fails on silence, on a
			// wrong sample rate, and on byte-order mistakes.
			amp := goertzel(interior(pcm.Samples), 1000, 16000)
			if amp < fixtureAmplitude/3 {
				t.Errorf("1 kHz amplitude = %.4f, want near %.4f", amp, fixtureAmplitude)
			}
			if amp > fixtureAmplitude*1.5 {
				t.Errorf("1 kHz amplitude = %.4f, well above the source %.4f — gain is wrong",
					amp, fixtureAmplitude)
			}
		})
	}
}

func TestFFmpegRejectsGarbage(t *testing.T) {
	d := newTestFFmpeg(t)

	_, err := d.Decode(context.Background(), bytes.NewReader([]byte("this is not audio at all")),
		Options{TargetSampleRate: 16000})
	if err == nil {
		t.Fatal("expected an error for non-audio input")
	}

	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeUnsupportedMediaType {
		t.Fatalf("got %v, want unsupported_media_type", err)
	}
	// ffmpeg's diagnosis belongs in the log, not in the client-facing message.
	if e.Message == "" || e.Unwrap() == nil {
		t.Error("ffmpeg stderr should be attached as the cause, not dropped")
	}
}

func TestFFmpegEnforcesDurationLimit(t *testing.T) {
	requireFFmpeg(t)
	d := newTestFFmpeg(t)
	raw := encodeWith(t, "-f", "mp3")

	_, err := d.Decode(context.Background(), bytes.NewReader(raw),
		Options{TargetSampleRate: 16000, MaxDurationSec: 0.2})

	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeDurationExceeded {
		t.Fatalf("got %v, want duration_exceeded", err)
	}
}

func TestFFmpegRespectsCancellation(t *testing.T) {
	d := newTestFFmpeg(t)
	raw := encodeWith(t, "-f", "mp3")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.Decode(ctx, bytes.NewReader(raw), Options{TargetSampleRate: 16000}); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}

// Without ffmpeg the server still serves WAV; it must not fail to start.
func TestFFmpegAbsentDegradesGracefully(t *testing.T) {
	for _, path := range []string{"", "definitely-not-a-real-binary-9f3a"} {
		d := NewFFmpegDecoder(path, time.Second)
		if d != nil {
			t.Fatalf("NewFFmpegDecoder(%q) = %v, want nil", path, d)
		}
		if d.CanDecode(FormatMP3) {
			t.Error("a nil decoder must not claim it can decode")
		}
		if _, err := d.Decode(context.Background(), bytes.NewReader(nil), Options{}); err == nil {
			t.Error("a nil decoder must return an error, not panic or succeed")
		}
	}
}

func TestRouterPrefersNativePathForWAV(t *testing.T) {
	requireFFmpeg(t)

	native := &countingDecoder{Decoder: NewWAVDecoder()}
	router := NewRouter(native, newTestFFmpeg(t))

	raw := buildWAV(wavPCM, 1, 16000, 16, pcm16(1000, -1000, 0))
	_, format, err := router.Decode(context.Background(), bytes.NewReader(raw),
		Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if format != FormatWAV {
		t.Errorf("format = %q, want wav", format)
	}
	if native.calls != 1 {
		t.Errorf("native decoder called %d times, want 1 — WAV must not pay for a process", native.calls)
	}
}

func TestRouterReportsUnsupportedWithoutFFmpeg(t *testing.T) {
	router := NewRouter(NewWAVDecoder()) // no ffmpeg registered

	mp3ish := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), make([]byte, 64)...)
	_, _, err := router.Decode(context.Background(), bytes.NewReader(mp3ish),
		Options{TargetSampleRate: 16000})

	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeUnsupportedMediaType {
		t.Fatalf("got %v, want unsupported_media_type", err)
	}
	// The operator has to be able to tell "install ffmpeg" from "that was not
	// audio"; both are 415 and only the message distinguishes them.
	if !strings.Contains(e.Message, "ffmpeg") {
		t.Errorf("message %q should point at the missing ffmpeg", e.Message)
	}
}

func TestRouterDistinguishesUnrecognisableInput(t *testing.T) {
	router := NewRouter(NewWAVDecoder(), newTestFFmpeg(t))

	_, _, err := router.Decode(context.Background(), bytes.NewReader([]byte("not audio at all")),
		Options{TargetSampleRate: 16000})

	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeUnsupportedMediaType {
		t.Fatalf("got %v, want unsupported_media_type", err)
	}
	if strings.Contains(e.Message, "ffmpeg") {
		t.Errorf("message %q blames ffmpeg for input that is not audio", e.Message)
	}
}

type countingDecoder struct {
	Decoder
	calls int
}

func (c *countingDecoder) Decode(ctx context.Context, r io.Reader, opts Options) (PCM, error) {
	c.calls++
	return c.Decoder.Decode(ctx, r, opts)
}
