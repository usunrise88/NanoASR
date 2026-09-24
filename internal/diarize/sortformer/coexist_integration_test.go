//go:build integration

package sortformer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/diarize"
	"github.com/usunrise88/nanoasr/internal/diarize/sherpa"
)

// The process has two onnxruntime environments over one library: sherpa-onnx's,
// created inside its C++ code, and ours. They must be able to run at the same
// time, since a server configured with backend: sherpa for diarization still
// has sherpa recognizers, and one configured with sortformer has both.
func TestCoexistsWithSherpa(t *testing.T) {
	models := filepath.Join("..", "..", "..", ".models")
	seg := filepath.Join(models, "pyannote-segmentation-3@3.0", "model.onnx")
	emb := filepath.Join(models, "campplus-sv-voxceleb@16k", "3dspeaker_speech_campplus_sv_en_voxceleb_16k.onnx")
	for _, f := range []string{seg, emb} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("no %s (run scripts/fetch-dev-models.sh)", f)
		}
	}
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "audio", "ru-16k.wav"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(context.Background(), f, audio.Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatal(err)
	}

	sd, err := sherpa.New(diarize.Config{SegmentationModel: seg, EmbeddingModel: emb, Threshold: 0.5,
		MinDurationOn: 0.3, MinDurationOff: 0.5}, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close()
	r := newTestRunner(t, 1)

	const frames = 40
	in := syntheticEmbeds(frames)
	want := make([]float32, frames*subsampling*numSpeakers)
	if err := r.step(context.Background(), in, frames, want); err != nil {
		t.Fatal(err)
	}
	wantTurns, err := sd.Process(context.Background(), pcms[0], 0)
	if err != nil {
		t.Fatal(err)
	}

	iterations := 1000
	if testing.Short() {
		iterations = 100
	}
	var done atomic.Bool
	var sherpaRuns atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !done.Load() {
			turns, err := sd.Process(context.Background(), pcms[0], 0)
			if err != nil {
				t.Error(err)
				return
			}
			if len(turns) != len(wantTurns) {
				t.Errorf("sherpa gave %d turns alongside our runs, %d alone", len(turns), len(wantTurns))
				return
			}
			sherpaRuns.Add(1)
		}
	}()
	got := make([]float32, len(want))
	for i := 0; i < iterations; i++ {
		if err := r.step(context.Background(), in, frames, got); err != nil {
			t.Fatal(err)
		}
		for k := range got {
			if got[k] != want[k] {
				t.Fatalf("iteration %d: a step next to sherpa differs from one alone", i)
			}
		}
	}
	done.Store(true)
	wg.Wait()
	t.Logf("%d steps, %d sherpa diarizations alongside", iterations, sherpaRuns.Load())
}
