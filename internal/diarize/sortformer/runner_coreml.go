package sortformer

import (
	"context"
	"fmt"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
)

// coremlRunner runs the two graphs as CoreML models on the Neural Engine.
//
// The Neural Engine runs fixed shapes only, so both are converted at the
// largest size the chunk loop asks for (scripts/coreml): the embedding graph
// takes a whole chunk with its look-ahead, the step graph the longest step,
// with a mask over its valid prefix. Shorter inputs are zero-padded here.
// Padding is harmless to the embedding graph, which projects each group of
// eight frames on its own; the step graph attends only to masked-in frames and
// zeroes the rest before its one convolution, so the rows that count come out
// as a dense input of their length would give them.
type coremlRunner struct {
	embedM, stepM *coreml.Model
	groups        int // embedding groups per embed call
	frames        int // encoder frames per step call
}

func newCoreMLRunner(embedPath, stepPath string) (*coremlRunner, error) {
	r := &coremlRunner{}
	var err error
	if r.embedM, err = coreml.Load(embedPath, "", coreml.NeuralEngine); err != nil {
		return nil, core.Errorf(core.CodeInternal, "sortformer: %s", err.Error()).WithCause(err)
	}
	if r.stepM, err = coreml.Load(stepPath, "", coreml.NeuralEngine); err != nil {
		r.Close()
		return nil, core.Errorf(core.CodeInternal, "sortformer: %s", err.Error()).WithCause(err)
	}
	feats, err1 := r.embedM.Description().Input("features")
	embeds, err2 := r.stepM.Description().Input("embeds")
	if err1 != nil || err2 != nil || len(feats.Shape) != 3 || len(embeds.Shape) != 3 ||
		feats.Shape[1]%subsampling != 0 || feats.Shape[2] != melBins || embeds.Shape[2] != hiddenSize {
		r.Close()
		return nil, core.Errorf(core.CodeInternal, "sortformer: %s and %s are not the converted graphs", embedPath, stepPath)
	}
	r.groups, r.frames = feats.Shape[1]/subsampling, embeds.Shape[1]
	return r, nil
}

func (r *coremlRunner) embed(feats []float32, frames int) ([]float32, error) {
	if frames%subsampling != 0 || len(feats) != frames*melBins {
		return nil, fmt.Errorf("embed: %d frames (%d values) is not a whole number of %d-frame groups",
			frames, len(feats), subsampling)
	}
	groups := frames / subsampling
	out := make([]float32, 0, groups*hiddenSize)
	in := make([]float32, r.groups*subsampling*melBins)
	res := make([]float32, r.groups*hiddenSize)
	for first := 0; first < groups; first += r.groups {
		n := min(r.groups, groups-first)
		clear(in)
		copy(in, feats[first*subsampling*melBins:(first+n)*subsampling*melBins])
		if err := r.embedM.Predict(map[string][]float32{"features": in}, map[string][]float32{"embeds": res}); err != nil {
			return nil, core.Errorf(core.CodeInternal, "embedding graph failed").WithCause(err)
		}
		out = append(out, res[:n*hiddenSize]...)
	}
	return out, nil
}

func (r *coremlRunner) step(ctx context.Context, embeds []float32, frames int, out []float32) error {
	if len(embeds) != frames*hiddenSize || len(out) != frames*subsampling*numSpeakers {
		return fmt.Errorf("step: %d frames do not match %d embedding values and %d logits",
			frames, len(embeds), len(out))
	}
	if frames > r.frames {
		return fmt.Errorf("step: %d frames, the converted graph takes at most %d", frames, r.frames)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	in := make([]float32, r.frames*hiddenSize)
	copy(in, embeds)
	mask := make([]float32, r.frames)
	for i := range frames {
		mask[i] = 1
	}
	logits := make([]float32, r.frames*subsampling*numSpeakers)
	if err := r.stepM.Predict(map[string][]float32{"embeds": in, "mask": mask},
		map[string][]float32{"logits": logits}); err != nil {
		return core.Errorf(core.CodeInternal, "diarization graph failed").WithCause(err)
	}
	copy(out, logits)
	return nil
}

func (r *coremlRunner) Close() error {
	if r.embedM != nil {
		r.embedM.Close()
		r.embedM = nil
	}
	if r.stepM != nil {
		r.stepM.Close()
		r.stepM = nil
	}
	return nil
}
