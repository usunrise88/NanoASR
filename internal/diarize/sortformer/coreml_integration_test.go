//go:build integration && darwin

package sortformer

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The CoreML build over the same recording as TestOfflineMatchesReference.
// It runs in fp16, so it is held to the decisions rather than to 1e-3: the
// speaker activity it reports may differ from the reference's only in frames
// close to the threshold, and in few of them.
func TestOfflineCoreMLMatchesReference(t *testing.T) {
	dir := os.Getenv("SORTFORMER_COREML_DIR")
	if dir == "" {
		t.Skip("SORTFORMER_COREML_DIR is not set")
	}
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

	m, err := loadModel(Files{
		Embed:      filepath.Join(dir, "embed.mlmodelc"),
		Step:       filepath.Join(dir, "step.mlmodelc"),
		MelFilters: filepath.Join(dir, "mel_filters.bin"),
		Silence:    filepath.Join(dir, "silence_embeds.bin"),
		Config:     filepath.Join(dir, "config.json"),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer m.r.Close()

	got := make([]float32, 0, golden.Frames*numSpeakers)
	start := time.Now()
	if err := m.run(context.Background(), x, func(_ int, rows []float32) {
		for _, v := range rows {
			got = append(got, sigmoid(v))
		}
	}); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	got = got[:golden.Frames*numSpeakers]

	worst, sum, flips, active := 0.0, 0.0, 0, 0
	for i, g := range got {
		d := math.Abs(float64(g) - float64(want[i]))
		worst = math.Max(worst, d)
		sum += d
		if (g > 0.5) != (want[i] > 0.5) {
			flips++
		}
		if want[i] > 0.5 {
			active++
		}
	}
	dur := float64(len(x)) / sampleRate
	t.Logf("%.1f s in %v (RTF %.4f): mean |Δp| %.3g, max %.3g; %d of %d frame decisions differ (%d active in the reference)",
		dur, took, took.Seconds()/dur, sum/float64(len(got)), worst, flips, len(got), active)
	if float64(flips) > 0.01*float64(active) {
		t.Errorf("%d decisions differ, more than 1%% of the %d active frames", flips, active)
	}
}
