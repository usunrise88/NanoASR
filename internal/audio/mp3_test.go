package audio

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestEncodeMP3(t *testing.T) {
	d := NewFFmpegDecoder("ffmpeg", 0)
	if d == nil {
		t.Skip("no ffmpeg")
	}
	var wav bytes.Buffer
	x := make([]float32, 32000)
	for i := range x {
		x[i] = float32(i%100) / 400
	}
	if err := WriteWAV(&wav, 16000, x); err != nil {
		t.Fatal(err)
	}
	out, err := d.EncodeMP3(context.Background(), io.NopCloser(&wav))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	// An MP3 opens with an ID3 tag or a frame sync.
	if len(b) < 1000 || (!bytes.HasPrefix(b, []byte("ID3")) && b[0] != 0xff) {
		t.Fatalf("%d bytes, starting %x: not an MP3", len(b), b[:min(4, len(b))])
	}
	// And it decodes back to about the same length.
	pcm, err := d.Decode(context.Background(), bytes.NewReader(b), Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pcm[0].Samples); n < 30000 || n > 36000 {
		t.Errorf("decodes back to %d samples for 32000", n)
	}
}

// A client that hangs up stops the encoder rather than leaving it running.
func TestEncodeMP3StopsOnClose(t *testing.T) {
	d := NewFFmpegDecoder("ffmpeg", 0)
	if d == nil {
		t.Skip("no ffmpeg")
	}
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(WAVHeader(16000, 1<<30))
		for {
			if _, err := pw.Write(make([]byte, 1<<16)); err != nil {
				return
			}
		}
	}()
	out, err := d.EncodeMP3(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(out, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
