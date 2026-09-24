//go:build integration

package sortformer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
)

func testFiles(t testing.TB) Files {
	dir := modelDir(t)
	return Files{
		Embed:      filepath.Join(dir, "embed.onnx"),
		Step:       filepath.Join(dir, "step.onnx"),
		MelFilters: filepath.Join(dir, "mel_filters.bin"),
		Silence:    filepath.Join(dir, "silence_embeds.bin"),
		Config:     configFile(dir),
	}
}

// configFile is the model's config.json, or for a bare export directory the
// golden reference it is packed from.
func configFile(dir string) string {
	if p := filepath.Join(dir, "config.json"); fileExists(p) {
		return p
	}
	return filepath.Join(goldenDir, "reference.json")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The whole pass over real speech — front end, embeddings, chunk loop, speaker
// cache, network — must reproduce the reference's speaker probabilities. The
// recording is long enough that the cache is compressed and then attended to.
func TestOfflineMatchesReference(t *testing.T) {
	var golden struct {
		Gap      int `json:"gap_samples"`
		Samples  int `json:"samples"`
		Frames   int `json:"frames"`
		Speakers int `json:"speakers"`
	}
	readJSON(t, "offline.json", &golden)
	want := readFloat32s(t, "offline.bin")

	speech := testSpeech(t)
	gap := make([]float32, golden.Gap)
	x := append(append(append(append(append([]float32{}, speech...), gap...), speech...), gap...), speech...)
	if len(x) != golden.Samples || numFrames(len(x)) != golden.Frames {
		t.Fatalf("%d samples, %d frames; reference %d, %d", len(x), numFrames(len(x)), golden.Samples, golden.Frames)
	}

	m, err := loadModel(testFiles(t), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer m.r.Close()

	got := make([]float32, 0, golden.Frames*numSpeakers)
	sink := func(first int, rows []float32) {
		for _, v := range rows {
			got = append(got, sigmoid(v))
		}
	}
	start := time.Now()
	if err := m.run(context.Background(), x, sink); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	got = got[:golden.Frames*numSpeakers]

	worst, flips, near := 0.0, 0, 0
	for i, g := range got {
		w := want[i]
		d := math.Abs(float64(g) - float64(w))
		worst = math.Max(worst, d)
		if (g > 0.5) != (w > 0.5) {
			if math.Abs(float64(w)-0.5) < 1e-3 {
				near++
				continue
			}
			flips++
			if flips <= 5 {
				t.Errorf("frame %d speaker %d: p=%.5f, reference %.5f", i/numSpeakers, i%numSpeakers, g, w)
			}
		}
	}
	if worst > 1e-3 {
		t.Errorf("max |Δp| = %.2g, want ≤ 1e-3", worst)
	}
	if flips > 0 {
		t.Errorf("%d decisions differ from the reference away from the threshold", flips)
	}
	dur := float64(len(x)) / sampleRate
	t.Logf("%.1f s: max |Δp| %.2g, %d decisions within 1e-3 of the threshold differ; %v, RTF %.4f",
		dur, worst, near, took, took.Seconds()/dur)
}

// Process end to end: turns in seconds, within the recording.
func TestProcessGivesTurns(t *testing.T) {
	files := testFiles(t)
	d, err := New(Options{Resolve: func(context.Context) (Files, error) { return files, nil }, NumThreads: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pcm := audio.PCM{Samples: testSpeech(t), SampleRate: sampleRate}
	turns, err := d.Process(context.Background(), pcm, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 {
		t.Fatal("no turns over speech")
	}
	for _, tr := range turns {
		if tr.Start < 0 || tr.End > pcm.Duration() || tr.End <= tr.Start || tr.Speaker != 0 {
			t.Errorf("turn %+v over a %.2f s single-speaker recording", tr, pcm.Duration())
		}
	}
	t.Logf("%d turns: %+v", len(turns), turns)
}
