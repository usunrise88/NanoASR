package sortformer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/usunrise88/nanoasr/internal/audio"
)

type frontendCase struct {
	Kind     string `json:"kind"`
	Samples  int    `json:"samples"`
	Frames   int    `json:"frames"`
	HeadRows int    `json:"head_rows"`
	TailRows int    `json:"tail_rows"`
	Offset   int    `json:"offset"`
}

// testSpeech is the fixture every golden vector was made from, decoded the way
// the service decodes it. It is fetched by scripts/fetch-testdata.sh, not
// committed, so tests that need it skip without it.
func testSpeech(t testing.TB) []float32 {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "audio", "ru-16k.wav"))
	if err != nil {
		t.Skipf("no speech fixture; run scripts/fetch-testdata.sh (%v)", err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(context.Background(), f, audio.Options{TargetSampleRate: sampleRate})
	if err != nil {
		t.Fatal(err)
	}
	return pcms[0].Samples
}

func (c frontendCase) signal(speech []float32) []float32 {
	x := make([]float32, c.Samples)
	for i := range x {
		switch c.Kind {
		case "speech":
			x[i] = speech[i%len(speech)]
		case "clip":
			x[i] = 1
			if (i/18)%2 == 1 {
				x[i] = -1
			}
		}
	}
	return x
}

func testFrontend(t testing.TB) *frontend {
	t.Helper()
	f, err := newFrontend(slaneyFilterbank())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The front end must reproduce the reference extractor's features: the same
// number of frames, and values within float32 noise of it. The reference runs
// its STFT in float32 and this one in float64, so they are not bit-identical:
// 1e-4 in log, ~0.01% in energy. Bins more than 14 nats below their frame's
// loudest are at the reference's own float32 floor — the square wave's gaps
// between harmonics, e⁻¹⁶ of the peak, move by 2e-4 between float32 and a
// float64 re-computation of the same recipe — and get 1e-3.
func TestFrontendMatchesReference(t *testing.T) {
	var golden struct {
		Cases []frontendCase `json:"cases"`
	}
	readJSON(t, "frontend.json", &golden)
	rows := readFloat32s(t, "frontend.bin")
	var speech []float32
	if _, err := os.Stat(filepath.Join("..", "..", "..", "testdata", "audio", "ru-16k.wav")); err == nil {
		speech = testSpeech(t)
	} else {
		t.Log("no speech fixture: checking silence and clipping only (run scripts/fetch-testdata.sh)")
	}
	fe := testFrontend(t)

	worst := 0.0
	for _, c := range golden.Cases {
		if c.Kind == "speech" && speech == nil {
			continue
		}
		x := c.signal(speech)
		if got := numFrames(len(x)); got != c.Frames {
			t.Errorf("%s/%d: %d frames, reference %d", c.Kind, c.Samples, got, c.Frames)
			continue
		}
		check := func(first, n, offset int) {
			got := make([]float32, n*melBins)
			fe.features(x, first, first+n, got)
			for i, g := range got {
				ref := rows[offset+i]
				peak := slices.Max(rows[offset+i/melBins*melBins : offset+(i/melBins+1)*melBins])
				tol := 1e-4
				if peak-ref > 14 {
					tol = 1e-3
				} else {
					worst = math.Max(worst, math.Abs(float64(g)-float64(ref)))
				}
				if d := math.Abs(float64(g) - float64(ref)); d > tol {
					t.Errorf("%s/%d frame %d bin %d: %g, reference %g", c.Kind, c.Samples,
						first+i/melBins, i%melBins, g, rows[offset+i])
					return
				}
			}
		}
		check(0, c.HeadRows, c.Offset)
		check(c.Frames-c.TailRows, c.TailRows, c.Offset+c.HeadRows*melBins)
	}
	t.Logf("max |Δ log-mel| %.2g above the float32 floor", worst)
}

// Computing the features a chunk at a time must give exactly what computing
// them all at once gives, whatever the chunk boundaries.
func TestFrontendChunksAreExact(t *testing.T) {
	x := make([]float32, 3*sampleRate+77)
	for i := range x {
		x[i] = float32(unit(uint64(i)) - 0.5)
	}
	fe := testFrontend(t)
	n := numFrames(len(x))
	whole := make([]float32, n*melBins)
	fe.features(x, 0, n, whole)

	for _, size := range []int{1, 7, 8, 2720} {
		for from := 0; from < n; from += size {
			to := min(from+size, n)
			part := make([]float32, (to-from)*melBins)
			fe.features(x, from, to, part)
			for i, v := range part {
				if v != whole[from*melBins+i] {
					t.Fatalf("chunks of %d: frame %d differs from the whole-file pass", size, from+i/melBins)
				}
			}
		}
	}
}

func BenchmarkFrontend(b *testing.B) {
	x := testSpeech(b)
	fe := testFrontend(b)
	n := numFrames(len(x))
	out := make([]float32, n*melBins)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fe.features(x, 0, n, out)
	}
	b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)/(float64(len(x))/sampleRate), "RTF")
}
