package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

			tracks, err := d.Decode(context.Background(), bytes.NewReader(raw),
				Options{TargetSampleRate: 16000, MaxDurationSec: 60})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(tracks) != 1 {
				t.Fatalf("decode returned %d tracks, want 1", len(tracks))
			}
			pcm := tracks[0]

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

func (c *countingDecoder) Decode(ctx context.Context, r io.Reader, opts Options) ([]PCM, error) {
	c.calls++
	return c.Decoder.Decode(ctx, r, opts)
}

// The split path is the only one where ffmpeg writes a container rather than
// raw samples, and the only one whose channel count is carried in the stream
// instead of in our arguments. A stereo fixture with a different tone per leg
// proves both that the channels arrive and that they did not get mixed.
func TestFFmpegSplitDeinterleavesChannels(t *testing.T) {
	d := newTestFFmpeg(t)

	// Left 1 kHz, right 3 kHz, merged into one stereo mp3.
	raw := encodeStereo(t, 1000, 3000, "-c:a", "libmp3lame", "-b:a", "64k", "-f", "mp3")

	tracks, err := d.Decode(context.Background(), bytes.NewReader(raw), Options{
		TargetSampleRate: 16000,
		ChannelMode:      core.ChannelSplit,
		MaxDurationSec:   60,
		MaxSplitChannels: 2,
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("split returned %d tracks, want 2", len(tracks))
	}

	for i, want := range []float64{1000, 3000} {
		other := []float64{3000, 1000}[i]
		samples := interior(tracks[i].Samples)
		mine := goertzel(samples, want, 16000)
		theirs := goertzel(samples, other, 16000)
		if mine < theirs*2 {
			t.Errorf("channel %d: %.0f Hz amplitude %.4f is not clearly above the %.0f Hz leak %.4f; "+
				"the channels were mixed", i, want, mine, other, theirs)
		}
		if tracks[i].Channel != i {
			t.Errorf("track %d reports Channel %d", i, tracks[i].Channel)
		}
	}
}

// first must keep one leg. -ac 1 would average them, which is downmix wearing
// the wrong name — the two produced identical output until M5.
func TestFFmpegFirstKeepsOneChannel(t *testing.T) {
	d := newTestFFmpeg(t)

	// Left 1 kHz, right silent.
	raw := encodeStereo(t, 1000, 0, "-c:a", "pcm_s16le", "-f", "wav")

	decode := func(mode core.ChannelMode) float64 {
		t.Helper()
		tracks, err := d.Decode(context.Background(), bytes.NewReader(raw), Options{
			TargetSampleRate: 16000, ChannelMode: mode, MaxDurationSec: 60,
		})
		if err != nil {
			t.Fatalf("decode %s: %v", mode, err)
		}
		return goertzel(interior(tracks[0].Samples), 1000, 16000)
	}

	first := decode(core.ChannelFirst)
	downmix := decode(core.ChannelDownmix)

	// Keeping the leg preserves its amplitude; mixing it with silence does not.
	// The gap is a factor of sqrt(2) rather than 2 because ffmpeg's -ac 1
	// normalises a stereo downmix by -3 dB instead of averaging — which is
	// exactly why the two modes cannot share an argument and why they produced
	// identical output until this was fixed.
	if first < fixtureAmplitude*0.8 {
		t.Errorf("first = %.4f, want near the full %.4f: the leg was not kept intact",
			first, fixtureAmplitude)
	}
	if first < downmix*1.25 {
		t.Errorf("first = %.4f, downmix = %.4f: first is not keeping the loud leg, it is averaging",
			first, downmix)
	}
}

// encodeStereo builds a two-channel fixture whose legs differ, so a test can
// tell one from the other after decoding. A zero frequency means silence.
func encodeStereo(t *testing.T, left, right int, args ...string) []byte {
	t.Helper()
	requireFFmpeg(t)

	source := func(hz int) string {
		if hz == 0 {
			return "anullsrc=r=16000:cl=mono:d=1"
		}
		return fmt.Sprintf("sine=frequency=%d:duration=1:sample_rate=16000", hz)
	}

	base := []string{"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", source(left),
		"-f", "lavfi", "-i", source(right),
		"-filter_complex", "[0:a][1:a]amerge=inputs=2[a]", "-map", "[a]"}
	cmd := exec.Command("ffmpeg", append(base, append(args, "pipe:1")...)...)

	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("encoding stereo fixture: %v\n%s", err, stderr.String())
	}
	return out.Bytes()
}
