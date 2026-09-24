package sortformer

import (
	"fmt"
	"math"
)

// The front end's parameters. They are the reference extractor's
// (NemotronAsrStreamingFeatureExtractor) and are checked against the model's
// config.json at load.
const (
	sampleRate   = 16000
	nFFT         = 512
	hopLength    = 160
	winLength    = 400
	preemphasis  = 0.97
	logZeroGuard = 1.0 / (1 << 24)
	freqBins     = nFFT/2 + 1
)

// frontend computes log-mel features the way the reference extractor does:
// preemphasis, a centred, zero-padded STFT under a symmetric 400-sample Hann
// window, the power spectrum, a Slaney mel filterbank and log(x + 2⁻²⁴).
//
// Frame t is a pure function of the samples within 200 of t·hop, so frames can
// be computed in any order and any grouping and come out bit for bit the same.
// The chunk loop uses that to compute only the frames of the chunk in hand
// rather than the whole recording's features (~740 MB at four hours).
//
// It holds no mutable state and is safe for concurrent use.
type frontend struct {
	window  [nFFT]float64
	filters []melFilter
	fft     *realFFT
}

// melFilter is one row of the filterbank, stored from its first non-zero bin:
// each of the 257 bins falls in at most two of the triangles.
type melFilter struct {
	first   int
	weights []float64
}

// newFrontend takes the filterbank as [melBins][freqBins], row-major.
func newFrontend(filterbank []float32) (*frontend, error) {
	if len(filterbank) != melBins*freqBins {
		return nil, fmt.Errorf("mel filterbank has %d values, want %d×%d", len(filterbank), melBins, freqBins)
	}
	f := &frontend{fft: newRealFFT(nFFT), filters: make([]melFilter, melBins)}
	// torch.hann_window(400, periodic=False), in float32 as the reference
	// computes it, centred in the 512-point frame as torch.stft pads it.
	off := (nFFT - winLength) / 2
	for n := 0; n < winLength; n++ {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(n)/float64(winLength-1))
		f.window[off+n] = float64(float32(w))
	}
	for m := range f.filters {
		row := filterbank[m*freqBins : (m+1)*freqBins]
		lo, hi := 0, freqBins
		for lo < hi && row[lo] == 0 {
			lo++
		}
		for hi > lo && row[hi-1] == 0 {
			hi--
		}
		w := make([]float64, hi-lo)
		for k := range w {
			w[k] = float64(row[lo+k])
		}
		f.filters[m] = melFilter{first: lo, weights: w}
	}
	return f, nil
}

// numFrames is how many feature frames a recording of samples has. The STFT
// yields one more; the reference zeroes and masks it.
func numFrames(samples int) int { return samples / hopLength }

// features writes frames [from, to) of x's log-mel features into out, which
// must hold (to-from)·melBins values, row-major.
func (f *frontend) features(x []float32, from, to int, out []float32) {
	if from < 0 || to > numFrames(len(x)) || from > to || len(out) != (to-from)*melBins {
		panic(fmt.Sprintf("frontend: frames [%d, %d) of %d into %d values", from, to, numFrames(len(x)), len(out)))
	}
	var frame [nFFT]float64
	var power [freqBins]float64
	var re, im [nFFT / 2]float64
	for t := from; t < to; t++ {
		// center=True: frame t covers samples [t·hop - n_fft/2, t·hop + n_fft/2)
		start := t*hopLength - nFFT/2
		for j := range frame {
			if f.window[j] == 0 {
				frame[j] = 0
				continue
			}
			frame[j] = float64(emphasized(x, start+j)) * f.window[j]
		}
		f.fft.power(frame[:], power[:], re[:], im[:])
		row := out[(t-from)*melBins : (t-from+1)*melBins]
		for m, flt := range f.filters {
			var s float64
			for k, w := range flt.weights {
				s += w * power[flt.first+k]
			}
			row[m] = float32(math.Log(s + logZeroGuard))
		}
	}
}

// emphasized is sample n after preemphasis, with the zero padding outside the
// recording. It is computed in float32, as the reference computes it.
func emphasized(x []float32, n int) float32 {
	switch {
	case n < 0 || n >= len(x):
		return 0
	case n == 0:
		return x[0]
	}
	return x[n] - float32(preemphasis)*x[n-1]
}

// slaneyFilterbank is librosa.filters.mel(sr, n_fft, n_mels, fmin=0,
// fmax=sr/2, norm="slaney") — the filterbank the reference extractor builds —
// reproduced step for step. The model ships its own copy; this one lets the
// front end be tested without a model on disk, and checks that copy.
func slaneyFilterbank() []float32 {
	const fSp = 200.0 / 3
	const minLogHz = 1000.0
	const minLogMel = minLogHz / fSp
	logStep := math.Log(6.4) / 27
	hzToMel := func(f float64) float64 {
		if f >= minLogHz {
			return minLogMel + math.Log(f/minLogHz)/logStep
		}
		return f / fSp
	}
	melToHz := func(m float64) float64 {
		if m >= minLogMel {
			return minLogHz * math.Exp(logStep*(m-minLogMel))
		}
		return fSp * m
	}

	// np.linspace(min_mel, max_mel, n_mels+2), then back to hertz
	n := melBins + 2
	minMel, maxMel := hzToMel(0), hzToMel(sampleRate/2)
	step := (maxMel - minMel) / float64(n-1)
	melF := make([]float64, n)
	for i := range melF {
		m := float64(i)*step + minMel
		if i == n-1 {
			m = maxMel
		}
		melF[i] = melToHz(m)
	}
	// np.fft.rfftfreq(n_fft, 1/sr)
	d := 1.0 / sampleRate
	val := 1.0 / (nFFT * d)

	out := make([]float32, melBins*freqBins)
	for i := 0; i < melBins; i++ {
		lowDiff, highDiff := melF[i+1]-melF[i], melF[i+2]-melF[i+1]
		enorm := 2.0 / (melF[i+2] - melF[i])
		for k := 0; k < freqBins; k++ {
			fk := float64(k) * val
			lower := -(melF[i] - fk) / lowDiff
			upper := (melF[i+2] - fk) / highDiff
			w := float32(math.Max(0, math.Min(lower, upper)))
			out[i*freqBins+k] = float32(float64(w) * enorm)
		}
	}
	return out
}
