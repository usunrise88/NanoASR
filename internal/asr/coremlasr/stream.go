package coremlasr

import (
	"github.com/usunrise88/nanoasr/internal/audio"
)

// StreamingOptions are the realtime settings a streaming CoreML model takes,
// the ones sherpa-onnx's streaming recogniser is configured with.
type StreamingOptions struct {
	EnableEndpoint          bool
	Rule1MinTrailingSilence float32
	Rule2MinTrailingSilence float32
	Rule3MinUtteranceLength float32
}

// endpointRule is one of sherpa-onnx's endpoint rules (endpoint.cc).
type endpointRule struct {
	mustContainNonSilence bool
	minTrailingSilence    float32
	minUtteranceLength    float32
}

// endpoints are the three rules, built the way sherpa-onnx's Go binding
// builds them from the three numbers NanoASR configures: each number replaces
// one field of the default rule, the others keep their defaults.
type endpoints struct {
	enabled bool
	rules   [3]endpointRule
}

func newEndpoints(o StreamingOptions) endpoints {
	return endpoints{
		enabled: o.EnableEndpoint,
		rules: [3]endpointRule{
			{false, o.Rule1MinTrailingSilence, 0},
			{true, o.Rule2MinTrailingSilence, 0},
			{false, 0, o.Rule3MinUtteranceLength},
		},
	}
}

// is reports whether an utterance this long, ending in this much silence, is
// over. Both in seconds.
func (e endpoints) is(utterance, trailingSilence float32) bool {
	if !e.enabled {
		return false
	}
	for _, r := range e.rules {
		nonSilence := utterance > trailingSilence
		if (nonSilence || !r.mustContainNonSilence) &&
			trailingSilence >= r.minTrailingSilence && utterance >= r.minUtteranceLength {
			return true
		}
	}
	return false
}

// streamResampler converts a stream that arrives in pieces, as Kaldi's
// LinearResample does inside sherpa-onnx: the output equals resampling the
// whole stream at once. audio.Resampler is stateless — it treats everything
// outside its buffer as silence — so this keeps a little history and emits an
// output sample only once every input sample its filter reaches has arrived.
type streamResampler struct {
	r       *audio.Resampler
	rateIn  int
	rateOut int
	unitIn  int // input samples per phase cycle
	unitOut int // output samples per phase cycle
	margin  int // input samples one output sample reaches on either side
	in      []float32
	inStart int // absolute index of in[0], a multiple of unitIn
	outDone int // output samples emitted so far
	inTotal int // input samples received so far
}

func newStreamResampler(rateIn, rateOut int) *streamResampler {
	g := gcd(rateIn, rateOut)
	lower := min(rateIn, rateOut)
	// Kaldi's filter half-width, in input samples: lowpass_filter_width zero
	// crossings of a sinc cut just under the lower Nyquist, plus slack.
	margin := int(6/(2*0.99*0.5*float64(lower))*float64(rateIn)) + 4
	return &streamResampler{
		r: audio.NewResampler(rateIn, rateOut), rateIn: rateIn, rateOut: rateOut,
		unitIn: rateIn / g, unitOut: rateOut / g, margin: margin,
	}
}

// push adds input and returns the output that is now final.
func (s *streamResampler) push(x []float32) []float32 {
	s.in = append(s.in, x...)
	s.inTotal += len(x)
	return s.emit(false)
}

// flush returns the rest, the stream's end treated as silence beyond it.
func (s *streamResampler) flush() []float32 { return s.emit(true) }

func (s *streamResampler) emit(final bool) []float32 {
	// Output n sits at input position n * rateIn / rateOut.
	var end int
	if final {
		end = s.r.OutputLen(s.inTotal)
	} else {
		end = max(0, (s.inTotal-s.margin)*s.rateOut/s.rateIn)
	}
	if end <= s.outDone {
		return nil
	}
	y := s.r.Resample(s.in)
	base := s.inStart / s.unitIn * s.unitOut // output index of y[0]
	lo, hi := s.outDone-base, min(end-base, len(y))
	var out []float32
	if lo >= 0 && hi > lo {
		out = append(out, y[lo:hi]...)
		s.outDone = base + hi
	}
	// Drop history no later output needs, keeping whole phase cycles.
	keepFrom := s.outDone*s.rateIn/s.rateOut - s.margin
	if drop := (keepFrom - s.inStart) / s.unitIn * s.unitIn; drop > 0 && drop <= len(s.in) {
		s.in = s.in[drop:]
		s.inStart += drop
	}
	return out
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
