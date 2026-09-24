//go:build integration

package sortformer

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelDir is where the exported graphs live: SORTFORMER_MODEL_DIR, or the
// registry's layout under .models. Tests that need them skip without them.
func modelDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("SORTFORMER_MODEL_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", ".models", "nemotron-3-diarization@2026-09-23")
	}
	for _, f := range []string{"embed.onnx", "step.onnx", "mel_filters.bin", "silence_embeds.bin"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Skipf("no %s in %s (set SORTFORMER_MODEL_DIR or run scripts/fetch-dev-models.sh)", f, dir)
		}
	}
	return dir
}

func newTestRunner(t testing.TB, threads int) *ortRunner {
	t.Helper()
	dir := modelDir(t)
	r, err := newORTRunner(filepath.Join(dir, "embed.onnx"), filepath.Join(dir, "step.onnx"),
		sessionTuning{IntraOpThreads: threads})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// syntheticEmbeds is a deterministic input the size of a full offline step.
func syntheticEmbeds(frames int) []float32 {
	out := make([]float32, frames*hiddenSize)
	for i := range out {
		out[i] = float32((unit(uint64(i)) - 0.5) * 16)
	}
	return out
}

// One session serves every job, so concurrent Runs on it must compute exactly
// what one Run alone computes.
func TestRunnerConcurrentStepsMatchSerial(t *testing.T) {
	r := newTestRunner(t, 2)
	const frames = 684
	in := syntheticEmbeds(frames)
	want := make([]float32, frames*subsampling*numSpeakers)
	if err := r.step(context.Background(), in, frames, want); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 3; k++ {
				got := make([]float32, len(want))
				if err := r.step(context.Background(), in, frames, got); err != nil {
					errs <- err
					return
				}
				for i := range got {
					if got[i] != want[i] {
						errs <- errors.New("a concurrent run differs from the serial one")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// A cancelled request must not wait out a step it no longer wants.
func TestRunnerCancelsAStep(t *testing.T) {
	r := newTestRunner(t, 1)
	const frames = 684
	in := syntheticEmbeds(frames)
	out := make([]float32, frames*subsampling*numSpeakers)

	start := time.Now()
	if err := r.step(context.Background(), in, frames, out); err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)

	ctx, cancel := context.WithTimeout(context.Background(), full/10)
	defer cancel()
	start = time.Now()
	err := r.step(ctx, in, frames, out)
	took := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's error", err)
	}
	if took > full/2 {
		t.Errorf("cancelled step took %v, a full one %v: Terminate did not interrupt it", took, full)
	}
	t.Logf("full step %v, cancelled after %v", full, took)
}

func BenchmarkStep684(b *testing.B) {
	for _, tc := range []struct {
		name string
		tune sessionTuning
	}{
		{"spinning", sessionTuning{IntraOpThreads: 4}},
		{"no-spinning", sessionTuning{IntraOpThreads: 4, NoSpinning: true}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			dir := modelDir(b)
			r, err := newORTRunner(filepath.Join(dir, "embed.onnx"), filepath.Join(dir, "step.onnx"), tc.tune)
			if err != nil {
				b.Fatal(err)
			}
			defer r.Close()
			const frames = 684
			in := syntheticEmbeds(frames)
			out := make([]float32, frames*subsampling*numSpeakers)
			if err := r.step(context.Background(), in, frames, out); err != nil { // warm up
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := r.step(context.Background(), in, frames, out); err != nil {
					b.Fatal(err)
				}
			}
			// one offline step advances the transcript by chunk_length frames
			b.ReportMetric(b.Elapsed().Seconds()/float64(b.N)/(340*0.08), "RTF")
		})
	}
}

// rssMB is the process's resident set, for the measurements below.
func rssMB(t testing.TB) float64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skip("no /proc/self/status")
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "VmRSS:" {
			kb, _ := strconv.ParseFloat(f[1], 64)
			return kb / 1024
		}
	}
	t.Skip("no VmRSS")
	return 0
}

// TestMeasureConcurrency reports what decides how sessions are shared: the
// memory of the weights against that of one Run's activations, and whether N
// jobs on one session go as fast as N jobs on N sessions. It asserts nothing;
// run it with -v when the graphs or onnxruntime change.
func TestMeasureConcurrency(t *testing.T) {
	if os.Getenv("SORTFORMER_MEASURE") == "" {
		t.Skip("set SORTFORMER_MEASURE=1")
	}
	const frames, jobs, threads, rounds = 684, 2, 2, 4
	in := syntheticEmbeds(frames)
	runAll := func(rs []*ortRunner) time.Duration {
		start := time.Now()
		var wg sync.WaitGroup
		for j := 0; j < jobs; j++ {
			wg.Add(1)
			go func(r *ortRunner) {
				defer wg.Done()
				out := make([]float32, frames*subsampling*numSpeakers)
				for k := 0; k < rounds; k++ {
					if err := r.step(context.Background(), in, frames, out); err != nil {
						t.Error(err)
						return
					}
				}
			}(rs[j])
		}
		wg.Wait()
		return time.Since(start)
	}

	base := rssMB(t)
	shared := newTestRunner(t, threads)
	loaded := rssMB(t)
	out := make([]float32, frames*subsampling*numSpeakers)
	if err := shared.step(context.Background(), in, frames, out); err != nil {
		t.Fatal(err)
	}
	oneRun := rssMB(t)
	sharedTook := runAll([]*ortRunner{shared, shared})
	concurrent := rssMB(t)

	other := newTestRunner(t, threads)
	if err := other.step(context.Background(), in, frames, out); err != nil {
		t.Fatal(err)
	}
	twoSessions := rssMB(t)
	separateTook := runAll([]*ortRunner{shared, other})

	t.Logf("RSS: before %.0f MB, sessions loaded +%.0f, after one Run +%.0f, after %d concurrent +%.0f, a second session +%.0f",
		base, loaded-base, oneRun-loaded, jobs, concurrent-oneRun, twoSessions-concurrent)
	t.Logf("%d jobs x %d steps, %d threads each: one shared session %v, separate sessions %v (shared/separate %.2f)",
		jobs, rounds, threads, sharedTook, separateTook, float64(separateTook)/float64(sharedTook))
}

// The filterbank the model ships is the one the front end's tests use.
func TestShippedFilterbankIsSlaney(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(modelDir(t), "mel_filters.bin"))
	if err != nil {
		t.Fatal(err)
	}
	want := slaneyFilterbank()
	if len(b) != 4*len(want) {
		t.Fatalf("mel_filters.bin is %d bytes, want %d", len(b), 4*len(want))
	}
	for i, w := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		if got != w {
			t.Fatalf("filter %d bin %d: shipped %g, computed %g", i/freqBins, i%freqBins, got, w)
		}
	}
}
