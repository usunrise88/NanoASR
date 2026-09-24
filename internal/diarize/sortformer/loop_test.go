package sortformer

import (
	"context"
	"fmt"
	"testing"
)

// fakeRunner is golden_sortformer.py's fake network: embedding element 0 is the
// recording frame it came from (-1 for silence), and the head's logits are
// fakeLogit of it. It records every step's input as those frame ids.
type fakeRunner struct {
	sc     scenario
	inputs [][]int
}

func (f *fakeRunner) embed([]float32, int) ([]float32, error) { panic("unused") }
func (f *fakeRunner) Close() error                            { return nil }

func (f *fakeRunner) step(_ context.Context, embeds []float32, frames int, out []float32) error {
	ids := make([]int, frames)
	for i := range ids {
		ids[i] = int(embeds[i*hiddenSize])
		for sub := 0; sub < subsampling; sub++ {
			for s := 0; s < numSpeakers; s++ {
				out[(i*subsampling+sub)*numSpeakers+s] = fakeLogit(f.sc, ids[i], sub, s)
			}
		}
	}
	f.inputs = append(f.inputs, ids)
	return nil
}

// referenceParams is the offline pass as reference.json describes it.
func referenceParams(t testing.TB, silence []float32) loopParams {
	t.Helper()
	var ref struct {
		ChunkLength       int     `json:"chunk_length"`
		ChunkRightContext int     `json:"chunk_right_context"`
		FIFOLength        int     `json:"fifo_length"`
		UpdatePeriod      int     `json:"speaker_cache_update_period"`
		CacheLength       int     `json:"speaker_cache_length"`
		SilenceSlots      int     `json:"speaker_cache_silence_frames_per_speaker"`
		ScoreThreshold    float64 `json:"prediction_score_threshold"`
		LatestBoost       float64 `json:"latest_frames_score_boost"`
		MinPositiveRate   float64 `json:"min_positive_scores_rate"`
		StrongRate        float64 `json:"strong_boost_rate"`
		WeakRate          float64 `json:"weak_boost_rate"`
	}
	readJSON(t, "reference.json", &ref)
	return loopParams{
		chunkLen: ref.ChunkLength, rightCtx: ref.ChunkRightContext, silence: silence,
		cache: newCacheParams(ref.CacheLength, ref.SilenceSlots, ref.FIFOLength, ref.UpdatePeriod,
			ref.ScoreThreshold, ref.LatestBoost, ref.MinPositiveRate, ref.StrongRate, ref.WeakRate),
	}.withMax()
}

func idEmbeds(from, to int, dst []float32) error {
	clear(dst)
	for f := from; f < to; f++ {
		dst[(f-from)*hiddenSize] = float32(f)
	}
	return nil
}

// The chunk loop and the speaker cache, driven by a network whose every output
// is known, must feed each step exactly the frames the reference fed it, keep
// exactly the frames it kept, and emit every frame's logits once, in order.
func TestOfflineLoopMatchesReference(t *testing.T) {
	var golden struct {
		Scenarios []scenario `json:"scenarios"`
	}
	readJSON(t, "cache.json", &golden)
	silence := make([]float32, hiddenSize)
	silence[0] = -1
	p := referenceParams(t, silence)

	for _, sc := range golden.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			fr := &fakeRunner{sc: sc}
			next := 0
			sink := func(first int, rows []float32) {
				if first != next {
					t.Fatalf("chunk starts at frame %d, want %d", first, next)
				}
				n := len(rows) / (subsampling * numSpeakers)
				for i := 0; i < n; i++ {
					for sub := 0; sub < subsampling; sub++ {
						for s := 0; s < numSpeakers; s++ {
							if got, want := rows[(i*subsampling+sub)*numSpeakers+s], fakeLogit(sc, first+i, sub, s); got != want {
								t.Fatalf("frame %d: logits of another frame", first+i)
							}
						}
					}
				}
				next += n
			}
			type state struct {
				cache, fifo []int
				compressed  bool
			}
			var states []state
			p := p
			p.afterUpdate = func(c *speakerCache) {
				ids := func(e []float32, n int) []int {
					out := make([]int, n)
					for i := range out {
						out[i] = int(e[i*hiddenSize])
					}
					return out
				}
				states = append(states, state{ids(c.embeds, c.n), ids(c.fifo, c.nFIFO), c.compressed})
			}
			if err := runOffline(context.Background(), fr, p, sc.Frames, idEmbeds, sink); err != nil {
				t.Fatal(err)
			}
			if next != sc.Frames {
				t.Fatalf("emitted %d frames of %d", next, sc.Frames)
			}
			if len(fr.inputs) != len(sc.Steps) {
				t.Fatalf("%d steps, reference %d", len(fr.inputs), len(sc.Steps))
			}
			for k, st := range sc.Steps {
				if d := firstDiff(fr.inputs[k], st.Input); d != "" {
					t.Errorf("step %d input: %s", k, d)
				}
				if d := firstDiff(states[k].cache, st.Cache); d != "" {
					t.Errorf("cache after step %d: %s", k, d)
				}
				if d := firstDiff(states[k].fifo, st.FIFO); d != "" {
					t.Errorf("FIFO after step %d: %s", k, d)
				}
				if states[k].compressed != st.Compressed {
					t.Errorf("after step %d compressed = %v, reference %v", k, states[k].compressed, st.Compressed)
				}
			}
		})
	}
}

func firstDiff(got, want []int) string {
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			return fmt.Sprintf("position %d is frame %d, reference %d (%v vs %v)", i, got[i], want[i],
				got[max(0, i-3):min(len(got), i+4)], want[max(0, i-3):min(len(want), i+4)])
		}
	}
	if len(got) != len(want) {
		return fmt.Sprintf("%d frames, reference %d", len(got), len(want))
	}
	return ""
}
