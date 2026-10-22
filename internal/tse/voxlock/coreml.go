package voxlock

import (
	"fmt"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/coreml"
)

// FamilyCoreML is the catalog family of the CoreML build; the ONNX one is
// "voxlock".
const FamilyCoreML = "coreml_voxlock"

// CoreMLFiles are the CoreML build's models on disk.
type CoreMLFiles struct {
	Enroll string // enroll.mlmodelc: the voice sample to its conditioning, fp32 on the GPU
	Front  string // front.mlmodelc: STFT, voice map, subband norms, context fusion, fp16 on the GPU
	Back   string // back.mlmodelc: the band-split RNN and its masks, fp16 on the Neural Engine
}

// coremlGraphs is graphs on CoreML: the same network as the ONNX chunk graph,
// cut in two where the Neural Engine stops being accurate enough.
//
// Measured over a minute of a meeting against the fp32 ONNX graphs, as the
// agreement (SI-SDR) of the isolated audio: the whole chunk on the Neural
// Engine 21.8 dB, this split 31.1 dB, the int8 ONNX graph the server shipped
// before 37.2 dB. The front's normalisations and attention lose precision on
// the Neural Engine and not on the GPU in the same fp16; the six recurrent
// blocks, which are where the time goes, stay on the Neural Engine.
//
// The graphs take a fixed number of frames per call. A stream calls them
// with exactly that many until it is flushed; the last call is padded, and the
// state after the padding is never read.
type coremlGraphs struct {
	enrollM, frontM, backM *coreml.Model
	k                      int // frames per call
	fe, fc                 int // enrollment frames of the magnitude and of the context
}

// LoadCoreML opens the CoreML build.
func LoadCoreML(f CoreMLFiles) (*Model, error) {
	g := &coremlGraphs{}
	var err error
	if g.enrollM, err = coreml.Load(f.Enroll, "", coreml.CPUAndGPU); err != nil {
		return nil, core.Errorf(core.CodeInternal, "voxlock: %s", err.Error()).WithCause(err)
	}
	if g.frontM, err = coreml.Load(f.Front, "", coreml.CPUAndGPU); err != nil {
		g.close()
		return nil, core.Errorf(core.CodeInternal, "voxlock: %s", err.Error()).WithCause(err)
	}
	if g.backM, err = coreml.Load(f.Back, "", coreml.NeuralEngine); err != nil {
		g.close()
		return nil, core.Errorf(core.CodeInternal, "voxlock: %s", err.Error()).WithCause(err)
	}
	wav, err1 := g.frontM.Description().Input("wav")
	mag, err2 := g.frontM.Description().Input("enroll_mag")
	ctx, err3 := g.frontM.Description().Input("context")
	if err1 != nil || err2 != nil || err3 != nil || len(wav.Shape) != 2 || len(mag.Shape) != 3 || len(ctx.Shape) != 3 {
		g.close()
		return nil, core.Errorf(core.CodeInternal, "voxlock: %s is not the converted front graph", f.Front)
	}
	g.k = (wav.Shape[1]-nfft)/hop + 1
	g.fe, g.fc = mag.Shape[2], ctx.Shape[2]
	return &Model{g: g}, nil
}

func (g *coremlGraphs) fixedFrames() int { return g.k }

func (g *coremlGraphs) enroll(sample []float32) (*Voice, error) {
	mag := make([]float32, NF*g.fe)
	ctx := make([]float32, contextDim*g.fc)
	err := g.enrollM.Predict(map[string][]float32{"enroll": sample},
		map[string][]float32{"enroll_mag": mag, "context": ctx})
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "voxlock enrollment graph failed").WithCause(err)
	}
	return &Voice{mag: mag, ctx: ctx, magDim: [3]int64{1, NF, int64(g.fe)}, ctxDim: [3]int64{1, contextDim, int64(g.fc)}}, nil
}

func (g *coremlGraphs) chunk(wav []float32, k int, v *Voice, h, c []float32) ([]float32, error) {
	if len(wav) != (k-1)*hop+nfft || k > g.k {
		return nil, fmt.Errorf("voxlock: %d samples is not %d frames, or more than the graph's %d", len(wav), k, g.k)
	}
	if v.magDim[2] != int64(g.fe) || v.ctxDim[2] != int64(g.fc) {
		return nil, fmt.Errorf("voxlock: the voice was enrolled for another graph")
	}
	in := make([]float32, (g.k-1)*hop+nfft)
	copy(in, wav)
	feat := make([]float32, bands*featureDim*g.k)
	re := make([]float32, NF*g.k)
	im := make([]float32, NF*g.k)
	if err := g.frontM.Predict(map[string][]float32{"wav": in, "enroll_mag": v.mag, "context": v.ctx},
		map[string][]float32{"feat": feat, "re": re, "im": im}); err != nil {
		return nil, core.Errorf(core.CodeInternal, "voxlock extraction graph failed").WithCause(err)
	}
	frames := make([]float32, g.k*nfft)
	hOut, cOut := make([]float32, stateSize), make([]float32, stateSize)
	if err := g.backM.Predict(map[string][]float32{"feat": feat, "re": re, "im": im, "h": h, "c": c},
		map[string][]float32{"frames": frames, "h_out": hOut, "c_out": cOut}); err != nil {
		return nil, core.Errorf(core.CodeInternal, "voxlock extraction graph failed").WithCause(err)
	}
	copy(h, hOut)
	copy(c, cOut)
	return frames[:k*nfft], nil
}

func (g *coremlGraphs) close() error {
	for _, m := range []**coreml.Model{&g.enrollM, &g.frontM, &g.backM} {
		if *m != nil {
			(*m).Close()
			*m = nil
		}
	}
	return nil
}

// The network's own dimensions, beside the STFT's in voxlock.go.
const (
	NF         = nfft/2 + 1 // STFT bins
	contextDim = 512        // the voice encoder's frame embedding
	bands      = lstmBatch  // the band split: one LSTM batch row per band
	featureDim = 128        // features per band
)
