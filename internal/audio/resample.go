package audio

import "math"

// Resampling on the native decode path.
//
// This is Kaldi's LinearResample algorithm — despite the name, a band-limited
// polyphase windowed-sinc resampler, not linear interpolation. The choice is
// deliberate: sherpa-onnx runs the same algorithm inside AcceptWaveform, so the
// signal the VAD sees and the signal the acoustic model sees are resampled
// identically instead of by two different filters.
//
// It matters most on the primary workload. Telephony arrives at 8 kHz and has
// to reach 16 kHz for the VAD; linear interpolation there folds imaging noise
// back into the speech band and costs measurable accuracy.

const (
	// filterWidth is Kaldi's lowpass_filter_width: the number of sinc zero
	// crossings kept on each side.
	filterWidth = 6
	// cutoffRatio is Kaldi's 0.99 * 0.5: just under Nyquist of the lower rate,
	// leaving a small transition band.
	cutoffRatio = 0.99 * 0.5
)

// Resampler converts between two fixed sample rates.
//
// Coefficients depend only on the rate pair, so one resampler is reused for
// every request at that pair. It holds no per-call state and is safe for
// concurrent use.
type Resampler struct {
	rateIn, rateOut int

	// A "unit" is the smallest block over which the phase pattern repeats:
	// rateIn/gcd input samples map to rateOut/gcd output samples.
	inputPerUnit  int
	outputPerUnit int

	firstIndex []int       // per output phase: index of its first input sample
	weights    [][]float64 // per output phase: filter taps
}

// NewResampler precomputes the polyphase filter bank for one rate pair.
func NewResampler(rateIn, rateOut int) *Resampler {
	g := gcd(rateIn, rateOut)
	r := &Resampler{
		rateIn:        rateIn,
		rateOut:       rateOut,
		inputPerUnit:  rateIn / g,
		outputPerUnit: rateOut / g,
	}

	minRate := rateIn
	if rateOut < minRate {
		minRate = rateOut
	}
	cutoff := cutoffRatio * float64(minRate)
	windowWidth := filterWidth / (2.0 * cutoff)

	r.firstIndex = make([]int, r.outputPerUnit)
	r.weights = make([][]float64, r.outputPerUnit)

	for i := range r.outputPerUnit {
		outputT := float64(i) / float64(rateOut)
		minIdx := int(math.Ceil((outputT - windowWidth) * float64(rateIn)))
		maxIdx := int(math.Floor((outputT + windowWidth) * float64(rateIn)))

		r.firstIndex[i] = minIdx
		taps := make([]float64, maxIdx-minIdx+1)
		for j := range taps {
			inputT := float64(minIdx+j) / float64(rateIn)
			taps[j] = filterFunc(inputT-outputT, cutoff, windowWidth) / float64(rateIn)
		}
		r.weights[i] = taps
	}
	return r
}

// filterFunc is a sinc at the given cutoff, multiplied by a Hann window. Both
// halves are symmetric in t, so the sign of the offset does not matter.
func filterFunc(t, cutoff, windowWidth float64) float64 {
	if math.Abs(t) >= windowWidth {
		return 0
	}
	window := 0.5 * (1 + math.Cos(math.Pi*t/windowWidth))

	if t == 0 {
		// Limit of sin(2πct)/(πt) as t → 0.
		return 2 * cutoff * window
	}
	return math.Sin(2*math.Pi*cutoff*t) / (math.Pi * t) * window
}

// OutputLen is how many samples Resample will produce for n input samples.
func (r *Resampler) OutputLen(n int) int {
	if n == 0 {
		return 0
	}
	// Every output index whose timestamp falls strictly inside the input
	// duration, which is what Kaldi's flush-mode resample emits.
	return int(math.Ceil(float64(n) * float64(r.rateOut) / float64(r.rateIn)))
}

// Resample converts in from rateIn to rateOut.
//
// Samples outside the input are treated as zero, which is the same edge
// handling Kaldi uses when flushing; the alternative — reflecting or holding —
// would put energy into the signal that was never recorded.
func (r *Resampler) Resample(in []float32) []float32 {
	if len(in) == 0 {
		return nil
	}
	out := make([]float32, r.OutputLen(len(in)))

	for n := range out {
		phase := n % r.outputPerUnit
		unit := n / r.outputPerUnit
		first := r.firstIndex[phase] + unit*r.inputPerUnit
		taps := r.weights[phase]

		// Clip the tap range to the available input instead of testing bounds
		// on every tap: this loop runs once per output sample.
		lo, hi := 0, len(taps)
		if first < 0 {
			lo = -first
		}
		if first+hi > len(in) {
			hi = len(in) - first
		}

		var sum float64
		for j := lo; j < hi; j++ {
			sum += float64(in[first+j]) * taps[j]
		}
		out[n] = float32(sum)
	}
	return out
}

// Resample converts a buffer between two rates, building the filter bank on the
// fly. Callers that resample repeatedly at the same rate pair should keep a
// Resampler instead.
func Resample(in []float32, from, to int) []float32 {
	if from == to || len(in) == 0 {
		return in
	}
	return NewResampler(from, to).Resample(in)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}
