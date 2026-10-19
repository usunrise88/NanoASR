package voxlock

import (
	"context"
	"math"
	"testing"
)

// identity is a graph that hands every frame back windowed the way the real
// one does, so the overlap-add must reconstruct the input exactly: whatever
// comes out that is not gain×input is a bug in the padding, the alignment or
// the normalisation, not in the network.
type identity struct {
	calls int
	// state counts the calls through h, to see that it crosses chunks.
	lastState float32
}

func (g *identity) enroll(sample []float32) (*Voice, error) { return &Voice{}, nil }

func (g *identity) chunk(wav []float32, k int, _ *Voice, h, c []float32) ([]float32, error) {
	g.calls++
	g.lastState = h[0]
	h[0]++
	out := make([]float32, k*nfft)
	for j := range k {
		for i := range nfft {
			out[j*nfft+i] = wav[j*hop+i] * win2[i]
		}
	}
	return out, nil
}

func (g *identity) close() error { return nil }

func signal(n int) []float32 {
	x := make([]float32, n)
	for i := range x {
		t := float64(i) / SampleRate
		x[i] = float32(0.3*math.Sin(2*math.Pi*220*t) + 0.1*math.Sin(2*math.Pi*1375*t+0.4))
	}
	return x
}

func TestIsolateReconstructsThroughAnIdentityGraph(t *testing.T) {
	for _, n := range []int{300, nfft, 4000, framesPerRun * hop, framesPerRun*hop + 1, 3*SampleRate + 77} {
		m := &Model{g: &identity{}}
		x := signal(n)
		y, err := m.Isolate(context.Background(), x, &Voice{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(y) != n {
			t.Fatalf("n=%d: got %d samples back", n, len(y))
		}
		var worst float64
		for i := range x {
			worst = max(worst, math.Abs(float64(y[i]-gain*x[i])))
		}
		if worst > 1e-5 {
			t.Errorf("n=%d: output differs from gain×input by up to %g", n, worst)
		}
	}
}

// The reference runs a call through push() in whatever pieces the transport
// delivers; the output must not depend on how the input was cut.
func TestStreamingMatchesWholeFile(t *testing.T) {
	x := signal(2*SampleRate + 333)
	whole, err := (&Model{g: &identity{}}).Isolate(context.Background(), x, &Voice{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []int{1, 100, 640, 1281} {
		s := (&Model{g: &identity{}}).NewStream(&Voice{})
		var got []float32
		for at := 0; at < len(x); at += piece {
			y, err := s.Push(x[at:min(at+piece, len(x))])
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, y...)
		}
		y, err := s.Flush()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, y...)
		if len(got) < len(x) {
			t.Fatalf("piece %d: %d samples out for %d in", piece, len(got), len(x))
		}
		for i := range x {
			if math.Abs(float64(got[i]-whole[i])) > 1e-6 {
				t.Fatalf("piece %d: sample %d is %g streamed, %g whole", piece, i, got[i], whole[i])
			}
		}
	}
}

// The output of a pushed piece trails the input by the frame the overlap-add
// is still waiting on, not by more: that is what makes a live call possible.
func TestStreamLatency(t *testing.T) {
	s := (&Model{g: &identity{}}).NewStream(&Voice{})
	in, out := 0, 0
	for range 50 {
		y, err := s.Push(make([]float32, 640))
		if err != nil {
			t.Fatal(err)
		}
		in += 640
		out += len(y)
		if lag := in - out; lag > nfft {
			t.Fatalf("after %d samples in, %d out: lag %d", in, out, lag)
		}
	}
}

func TestStateCrossesChunks(t *testing.T) {
	g := &identity{}
	m := &Model{g: g}
	if _, err := m.Isolate(context.Background(), signal(5*SampleRate), &Voice{}, nil); err != nil {
		t.Fatal(err)
	}
	if g.calls < 3 || g.lastState != float32(g.calls-1) {
		t.Errorf("%d calls, the last saw state %g: the LSTM state is not carried", g.calls, g.lastState)
	}
}

func TestIsolateStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	m := &Model{g: &identity{}}
	_, err := m.Isolate(ctx, signal(10*SampleRate), &Voice{}, func(int) {
		calls++
		cancel()
	})
	if err == nil || calls != 1 {
		t.Fatalf("err %v after %d progress calls; want a cancellation after the first", err, calls)
	}
}

func TestEnrollWantsThreeSeconds(t *testing.T) {
	m := &Model{g: &identity{}}
	if _, err := m.Enroll(make([]float32, EnrollSamples-1)); err == nil {
		t.Error("a sample one short of 3 s was accepted")
	}
	if _, err := m.Enroll(make([]float32, EnrollSamples)); err != nil {
		t.Error(err)
	}
}
