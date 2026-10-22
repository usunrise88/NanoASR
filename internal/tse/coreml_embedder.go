package tse

import (
	"slices"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
)

// FamilyCoreMLCampplus is the catalog family of CAM++'s CoreML build.
const FamilyCoreMLCampplus = "coreml_campplus"

// Kaldi framing as sherpa-onnx configures it for speaker embeddings:
// 25 ms windows every 10 ms, centred, the signal reflected at its ends.
const (
	kaldiFrame = 400
	kaldiShift = 160
)

// CoreMLEmbedder is CAM++ in two CoreML models: the Kaldi filterbank on the
// GPU in fp32, then the network on the Neural Engine in fp16.
//
// The split is measured, not taste. The network alone on the Neural Engine
// stays within a cosine of 0.9992 of sherpa-onnx's embedding; with the
// filterbank on the Neural Engine too it fell to 0.968, because fp16 cannot
// hold a power spectrum's dynamic range.
//
// The network is converted at a few fixed lengths with the true length passed
// as a mask, every pooling taken over the masked-in frames only, so a short
// input embeds as itself and not as itself plus silence. An input longer than
// the longest is embedded in pieces whose embeddings are averaged by length.
type CoreMLEmbedder struct {
	fbank   *coreml.Model
	nets    map[int]*coreml.Model
	buckets []int // frames, ascending
	dim     int
}

// NewCoreMLEmbedder loads the filterbank model and every function of the
// network model, whose names are "t" and the frame count.
func NewCoreMLEmbedder(fbankPath, netPath string) (*CoreMLEmbedder, error) {
	fb, err := coreml.Load(fbankPath, "", coreml.CPUAndGPU)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "speaker embedding: %s", err.Error()).WithCause(err)
	}
	e := &CoreMLEmbedder{fbank: fb, nets: map[int]*coreml.Model{}}
	in, err := fb.Description().Input("frames")
	if err != nil || len(in.Shape) != 2 || in.Shape[1] != kaldiFrame {
		e.Close()
		return nil, core.Errorf(core.CodeInternal, "speaker embedding: %s is not the converted filterbank", fbankPath)
	}
	for _, t := range []int{200, 400, 600, 1000, 2000} {
		if t > in.Shape[0] {
			break
		}
		m, err := coreml.Load(netPath, "t"+strconv.Itoa(t), coreml.NeuralEngine)
		if err != nil {
			if strings.Contains(err.Error(), "function") {
				continue
			}
			e.Close()
			return nil, core.Errorf(core.CodeInternal, "speaker embedding: %s", err.Error()).WithCause(err)
		}
		out, err := m.Description().Output("embedding")
		if err != nil {
			m.Close()
			e.Close()
			return nil, core.Errorf(core.CodeInternal, "speaker embedding: %s has no embedding output", netPath)
		}
		e.nets[t], e.dim = m, out.Size()
		e.buckets = append(e.buckets, t)
	}
	if len(e.buckets) == 0 {
		e.Close()
		return nil, core.Errorf(core.CodeInternal, "speaker embedding: %s has no network functions", netPath)
	}
	return e, nil
}

// Embed returns the unit-length embedding of samples at SampleRate.
func (e *CoreMLEmbedder) Embed(samples []float32) ([]float32, error) {
	frames := kaldiFrames(samples)
	if frames == 0 {
		// sherpa-onnx's answer to too little audio: no evidence either way.
		return make([]float32, e.dim), nil
	}
	longest := e.buckets[len(e.buckets)-1]
	if frames <= longest {
		return e.embed(samples, frames)
	}
	// Longer than the network was converted for: equal pieces, averaged.
	n := (frames + longest - 1) / longest
	size := (len(samples) + n - 1) / n
	sum := make([]float32, e.dim)
	for i := 0; i < len(samples); i += size {
		piece := samples[i:min(i+size, len(samples))]
		f := kaldiFrames(piece)
		if f == 0 {
			continue
		}
		v, err := e.embed(piece, f)
		if err != nil {
			return nil, err
		}
		for j := range v {
			sum[j] += v[j] * float32(f)
		}
	}
	return normalise(sum), nil
}

func (e *CoreMLEmbedder) embed(samples []float32, frames int) ([]float32, error) {
	in, _ := e.fbank.Description().Input("frames")
	capacity := in.Shape[0]
	buf := make([]float32, capacity*kaldiFrame)
	n := len(samples)
	for f := range frames {
		row := buf[f*kaldiFrame : (f+1)*kaldiFrame]
		start := f*kaldiShift - (kaldiFrame-kaldiShift)/2
		for i := range row {
			row[i] = samples[reflect(start+i, n)]
		}
	}
	fb := make([]float32, capacity*80)
	if err := e.fbank.Predict(map[string][]float32{"frames": buf}, map[string][]float32{"fbank": fb}); err != nil {
		return nil, core.Errorf(core.CodeInternal, "speaker embedding filterbank failed").WithCause(err)
	}
	t := e.buckets[slices.IndexFunc(e.buckets, func(b int) bool { return b >= frames })]
	feats := make([]float32, t*80)
	copy(feats, fb[:frames*80])
	mask := make([]float32, t)
	for i := range frames {
		mask[i] = 1
	}
	out := make([]float32, e.dim)
	if err := e.nets[t].Predict(map[string][]float32{"fbank": feats, "mask": mask},
		map[string][]float32{"embedding": out}); err != nil {
		return nil, core.Errorf(core.CodeInternal, "speaker embedding network failed").WithCause(err)
	}
	return normalise(out), nil
}

// kaldiFrames is how many frames Kaldi's centred framing makes of n samples.
func kaldiFrames(samples []float32) int {
	return (len(samples) + kaldiShift/2) / kaldiShift
}

// reflect folds an index outside [0, n) back in the way Kaldi does when its
// frames overhang the signal: the first sample is not repeated.
func reflect(i, n int) int {
	for i < 0 || i >= n {
		if i < 0 {
			i = -i - 1
		} else {
			i = 2*n - 1 - i
		}
	}
	return i
}

// Close frees the models.
func (e *CoreMLEmbedder) Close() {
	if e.fbank != nil {
		e.fbank.Close()
		e.fbank = nil
	}
	for t, m := range e.nets {
		m.Close()
		delete(e.nets, t)
	}
}
