package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

// buildWAV assembles a minimal RIFF file. extra chunks, when present, are
// written between the fmt and data chunks so tests can cover the skip path.
func buildWAV(tag, channels, rate, bits int, data []byte, extra ...[]byte) []byte {
	blockAlign := channels * bits / 8

	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:2], uint16(tag))
	binary.LittleEndian.PutUint16(fmtBody[2:4], uint16(channels))
	binary.LittleEndian.PutUint32(fmtBody[4:8], uint32(rate))
	binary.LittleEndian.PutUint32(fmtBody[8:12], uint32(rate*blockAlign))
	binary.LittleEndian.PutUint16(fmtBody[12:14], uint16(blockAlign))
	binary.LittleEndian.PutUint16(fmtBody[14:16], uint16(bits))

	if tag == wavExtensible {
		ext := make([]byte, 24)
		binary.LittleEndian.PutUint16(ext[0:2], 22) // cbSize
		binary.LittleEndian.PutUint16(ext[2:4], uint16(bits))
		binary.LittleEndian.PutUint32(ext[4:8], 0x3) // channel mask
		binary.LittleEndian.PutUint16(ext[8:10], wavPCM)
		fmtBody = append(fmtBody, ext...)
	}

	var body bytes.Buffer
	writeChunk(&body, "fmt ", fmtBody)
	for _, e := range extra {
		body.Write(e)
	}
	writeChunk(&body, "data", data)

	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(body.Len()+4))
	out.WriteString("WAVE")
	out.Write(body.Bytes())
	return out.Bytes()
}

func writeChunk(w *bytes.Buffer, id string, body []byte) {
	w.WriteString(id)
	_ = binary.Write(w, binary.LittleEndian, uint32(len(body)))
	w.Write(body)
	if len(body)%2 == 1 {
		w.WriteByte(0)
	}
}

func pcm16(vals ...int16) []byte {
	b := make([]byte, len(vals)*2)
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func decodeWAV(t *testing.T, raw []byte, opts Options) PCM {
	t.Helper()
	if opts.TargetSampleRate == 0 {
		opts.TargetSampleRate = 16000
	}
	pcm, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw), opts)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pcm) != 1 {
		t.Fatalf("decode returned %d tracks, want 1", len(pcm))
	}
	return pcm[0]
}

func TestWAVSampleFormats(t *testing.T) {
	// Each format encodes roughly +0.5, -0.5 and 0 full scale, so one table
	// covers every conversion path.
	cases := []struct {
		name string
		tag  int
		bits int
		data []byte
		want []float32
	}{
		{"pcm16", wavPCM, 16, pcm16(16384, -16384, 0), []float32{0.5, -0.5, 0}},
		{"pcm8", wavPCM, 8, []byte{192, 64, 128}, []float32{0.5, -0.5, 0}},
		{"pcm24", wavPCM, 24, []byte{0, 0, 0x40, 0, 0, 0xC0, 0, 0, 0}, []float32{0.5, -0.5, 0}},
		{"pcm32", wavPCM, 32, []byte{
			0, 0, 0, 0x40,
			0, 0, 0, 0xC0,
			0, 0, 0, 0,
		}, []float32{0.5, -0.5, 0}},
		{"float32", wavIEEEFloat, 32, floatBytes(0.5, -0.5, 0), []float32{0.5, -0.5, 0}},
		{"extensible-pcm16", wavExtensible, 16, pcm16(16384, -16384, 0), []float32{0.5, -0.5, 0}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := buildWAV(c.tag, 1, 16000, c.bits, c.data)
			got := decodeWAV(t, raw, Options{TargetSampleRate: 16000})

			if len(got.Samples) != len(c.want) {
				t.Fatalf("got %d samples, want %d", len(got.Samples), len(c.want))
			}
			for i, w := range c.want {
				if math.Abs(float64(got.Samples[i]-w)) > 0.01 {
					t.Errorf("sample %d = %.4f, want %.4f", i, got.Samples[i], w)
				}
			}
		})
	}
}

// A-law and mu-law are the formats that actually arrive from a PBX, so they get
// a round-trip check rather than a spot value.
func TestWAVCompandedFormatsRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name  string
		tag   int
		table *[256]int16
	}{
		{"alaw", wavALaw, &alawTable},
		{"mulaw", wavMuLaw, &ulawTable},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := make([]byte, 256)
			for i := range data {
				data[i] = byte(i)
			}
			raw := buildWAV(c.tag, 1, 8000, 8, data)
			got := decodeWAV(t, raw, Options{TargetSampleRate: 8000})

			if len(got.Samples) != 256 {
				t.Fatalf("got %d samples, want 256", len(got.Samples))
			}
			for i, s := range got.Samples {
				want := float32(c.table[i]) / 32768
				if math.Abs(float64(s-want)) > 1e-6 {
					t.Fatalf("byte %d decoded to %.6f, want %.6f", i, s, want)
				}
				if s < -1.001 || s > 1.001 {
					t.Fatalf("byte %d decoded outside [-1,1]: %.6f", i, s)
				}
			}
		})
	}
}

func TestWAVChannelModes(t *testing.T) {
	// Two channels: left at full scale, right at zero.
	raw := buildWAV(wavPCM, 2, 16000, 16, pcm16(16384, 0, 16384, 0))

	downmix := decodeWAV(t, raw, Options{TargetSampleRate: 16000, ChannelMode: core.ChannelDownmix})
	if math.Abs(float64(downmix.Samples[0]-0.25)) > 0.01 {
		t.Errorf("downmix = %.4f, want 0.25 (average of 0.5 and 0)", downmix.Samples[0])
	}

	first := decodeWAV(t, raw, Options{TargetSampleRate: 16000, ChannelMode: core.ChannelFirst})
	if math.Abs(float64(first.Samples[0]-0.5)) > 0.01 {
		t.Errorf("first channel = %.4f, want 0.5", first.Samples[0])
	}
	if first.SourceChannels != 2 {
		t.Errorf("SourceChannels = %d, want the source count 2", first.SourceChannels)
	}
}

func TestWAVSkipsUnknownChunks(t *testing.T) {
	// An odd-sized LIST chunk exercises both the skip path and the pad byte.
	var list bytes.Buffer
	writeChunk(&list, "LIST", []byte("INFOsome odd metadata"))

	raw := buildWAV(wavPCM, 1, 16000, 16, pcm16(16384, -16384), list.Bytes())
	got := decodeWAV(t, raw, Options{TargetSampleRate: 16000})

	if len(got.Samples) != 2 {
		t.Fatalf("got %d samples, want 2 — the LIST chunk was not skipped cleanly", len(got.Samples))
	}
}

func TestWAVResamplesToTarget(t *testing.T) {
	// 8 kHz telephony in, 16 kHz out: the length must double.
	data := pcm16(make([]int16, 800)...)
	raw := buildWAV(wavPCM, 1, 8000, 16, data)
	got := decodeWAV(t, raw, Options{TargetSampleRate: 16000})

	if got.SampleRate != 16000 {
		t.Fatalf("SampleRate = %d, want 16000", got.SampleRate)
	}
	if len(got.Samples) != 1600 {
		t.Fatalf("got %d samples, want 1600", len(got.Samples))
	}
}

func TestWAVRejections(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		opts Options
		code core.Code
	}{
		{
			name: "unknown format tag",
			raw:  buildWAV(0x0011, 1, 16000, 16, pcm16(0, 0)), // IMA ADPCM
			code: core.CodeUnsupportedMediaType,
		},
		{
			name: "not a wave file",
			raw:  []byte("RIFFxxxxAVI LIST"),
			code: core.CodeInvalidRequest,
		},
		{
			name: "truncated header",
			raw:  []byte("RIFF"),
			code: core.CodeInvalidRequest,
		},
		{
			name: "no data chunk",
			raw:  buildWAV(wavPCM, 1, 16000, 16, nil)[:36],
			code: core.CodeInvalidRequest,
		},
		{
			name: "empty data chunk",
			raw:  buildWAV(wavPCM, 1, 16000, 16, nil),
			code: core.CodeInvalidRequest,
		},
		{
			name: "exceeds duration limit",
			raw:  buildWAV(wavPCM, 1, 16000, 16, pcm16(make([]int16, 32000)...)),
			opts: Options{TargetSampleRate: 16000, MaxDurationSec: 1},
			code: core.CodeDurationExceeded,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			if opts.TargetSampleRate == 0 {
				opts.TargetSampleRate = 16000
			}
			_, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(c.raw), opts)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			var e *core.Error
			if !errors.As(err, &e) {
				t.Fatalf("error is not a domain error: %v", err)
			}
			if e.Code != c.code {
				t.Errorf("code = %q, want %q (%v)", e.Code, c.code, err)
			}
		})
	}
}

func TestWAVRespectsCancellation(t *testing.T) {
	raw := buildWAV(wavPCM, 1, 16000, 16, pcm16(make([]int16, 200000)...))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&WAVDecoder{}).Decode(ctx, bytes.NewReader(raw), Options{TargetSampleRate: 16000})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// FuzzWAVHeader guards the chunk walk: it must reject malformed input rather
// than panic or allocate unboundedly on an attacker-chosen chunk size.
func FuzzWAVHeader(f *testing.F) {
	f.Add(buildWAV(wavPCM, 1, 16000, 16, pcm16(1, 2, 3)))
	f.Add(buildWAV(wavALaw, 1, 8000, 8, []byte{0x55, 0xD5}))
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVE"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw),
			Options{TargetSampleRate: 16000, MaxDurationSec: 60})
	})
}

func floatBytes(vals ...float32) []byte {
	b := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

func TestWAVSplitKeepsChannelsApart(t *testing.T) {
	// Two channels: left at full scale, right at zero. Under downmix these
	// average to 0.25; under split neither value may move.
	raw := buildWAV(wavPCM, 2, 16000, 16, pcm16(16384, 0, 16384, 0))

	tracks, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw),
		Options{TargetSampleRate: 16000, ChannelMode: core.ChannelSplit, MaxSplitChannels: 2})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("split returned %d tracks, want one per channel", len(tracks))
	}
	for i, want := range []float32{0.5, 0} {
		if got := tracks[i].Samples[0]; math.Abs(float64(got-want)) > 0.01 {
			t.Errorf("channel %d sample = %.4f, want %.4f", i, got, want)
		}
		if tracks[i].Channel != i {
			t.Errorf("track %d reports Channel %d", i, tracks[i].Channel)
		}
		if tracks[i].SourceChannels != 2 {
			t.Errorf("track %d reports SourceChannels %d, want 2", i, tracks[i].SourceChannels)
		}
	}
}

// A mono file asked to split is one track, not an error: the caller wants every
// channel, and there is one.
func TestWAVSplitOfMonoIsOneTrack(t *testing.T) {
	raw := buildWAV(wavPCM, 1, 16000, 16, pcm16(16384, -16384))

	tracks, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw),
		Options{TargetSampleRate: 16000, ChannelMode: core.ChannelSplit, MaxSplitChannels: 2})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(tracks))
	}
}

// Refusing is the point: a sixteen-track session would otherwise be decoded in
// full, sixteen times the memory, before anyone noticed.
func TestWAVSplitRefusesTooManyChannels(t *testing.T) {
	raw := buildWAV(wavPCM, 4, 16000, 16, pcm16(0, 0, 0, 0))

	_, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw),
		Options{TargetSampleRate: 16000, ChannelMode: core.ChannelSplit, MaxSplitChannels: 2})
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeInvalidRequest {
		t.Fatalf("err = %v, want invalid_request naming the channel limit", err)
	}
	if e.Param != "channel_mode" {
		t.Errorf("Param = %q, want channel_mode so the caller knows which field to change", e.Param)
	}
}

// The memory ceiling counts every channel, which is the whole reason it exists
// beside the duration limit.
func TestWAVSplitRefusesOverMemoryBudget(t *testing.T) {
	frames := 4096
	data := make([]byte, frames*2*2) // 2 channels, 16-bit
	raw := buildWAV(wavPCM, 2, 16000, 16, data)

	// One channel of this fits; two do not.
	budget := int64(frames)*4 + 1

	_, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw),
		Options{
			TargetSampleRate: 16000,
			ChannelMode:      core.ChannelSplit,
			MaxSplitChannels: 2,
			MaxDecodedBytes:  budget,
		})
	var e *core.Error
	if !errors.As(err, &e) || e.Code != core.CodeFileTooLarge {
		t.Fatalf("err = %v, want file_too_large", err)
	}

	// The same file downmixed is one track and stays inside the budget.
	if _, err := (&WAVDecoder{}).Decode(context.Background(), bytes.NewReader(raw),
		Options{
			TargetSampleRate: 16000,
			ChannelMode:      core.ChannelDownmix,
			MaxDecodedBytes:  budget,
		}); err != nil {
		t.Fatalf("downmix of the same file must fit the budget: %v", err)
	}
}
