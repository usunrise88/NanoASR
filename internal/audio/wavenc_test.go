package audio

import (
	"bytes"
	"context"
	"math"
	"testing"
)

// What the encoder writes, the decoder reads back: the two have to agree on
// every field, or the isolated voice is a file nothing else can open either.
func TestWAVRoundTrip(t *testing.T) {
	in := make([]float32, 16000)
	for i := range in {
		in[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/16000))
	}
	in[10], in[11] = 1.5, -1.5 // clipped, not wrapped

	var buf bytes.Buffer
	if err := WriteWAV(&buf, 16000, in); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != WAVHeaderSize+2*len(in) {
		t.Fatalf("%d bytes", buf.Len())
	}
	pcm, err := NewWAVDecoder().Decode(context.Background(), &buf, Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatal(err)
	}
	out := pcm[0].Samples
	if len(out) != len(in) {
		t.Fatalf("%d samples back for %d", len(out), len(in))
	}
	for i := range in {
		want := max(-1, min(1, in[i]))
		if math.Abs(float64(out[i]-want)) > 1.0/16384 {
			t.Fatalf("sample %d: %g back for %g", i, out[i], want)
		}
	}
}
