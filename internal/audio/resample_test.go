package audio

import (
	"math"
	"testing"
)

func sine(freq, rate float64, n int, amp float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = amp * float32(math.Sin(2*math.Pi*freq*float64(i)/rate))
	}
	return out
}

// goertzel returns the amplitude of one frequency bin. It is a few lines and
// avoids pulling an FFT into the test just to check filter behaviour.
func goertzel(x []float32, freq, rate float64) float64 {
	if len(x) == 0 {
		return 0
	}
	coeff := 2 * math.Cos(2*math.Pi*freq/rate)
	var s0, s1, s2 float64
	for _, v := range x {
		s0 = float64(v) + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	power := s1*s1 + s2*s2 - coeff*s1*s2
	if power < 0 {
		power = 0
	}
	return 2 * math.Sqrt(power) / float64(len(x))
}

func db(ratio float64) float64 {
	if ratio <= 0 {
		return -200
	}
	return 20 * math.Log10(ratio)
}

// interior drops the filter's edge transient so the measurement reflects the
// steady state rather than the ramp-up.
func interior(x []float32) []float32 {
	pad := len(x) / 10
	return x[pad : len(x)-pad]
}

func TestResamplePreservesPassbandAmplitude(t *testing.T) {
	// 300 Hz sits well inside the passband for every pair below, so unity gain
	// there is the baseline every other test is measured against.
	for _, c := range []struct{ from, to int }{
		{8000, 16000},
		{16000, 8000},
		{44100, 16000},
		{16000, 16000},
	} {
		in := sine(300, float64(c.from), c.from, 0.5)
		out := Resample(in, c.from, c.to)

		got := goertzel(interior(out), 300, float64(c.to))
		if math.Abs(got-0.5) > 0.02 {
			t.Errorf("%d→%d Hz: 300 Hz amplitude = %.4f, want 0.5 (gain error %.2f dB)",
				c.from, c.to, got, db(got/0.5))
		}
	}
}

// Upsampling 8 kHz telephony to 16 kHz is the primary path: the VAD needs
// 16 kHz and the source is narrowband. Linear interpolation would leave a
// strong image of every tone mirrored about 4 kHz; a band-limited filter must
// not.
func TestResampleUpsamplingSuppressesImages(t *testing.T) {
	const (
		tone    = 1000.0
		rateIn  = 8000.0
		rateOut = 16000.0
		imageAt = rateIn - tone // 7000 Hz
		floorDB = -40.0
	)

	in := sine(tone, rateIn, 8000, 0.5)
	out := interior(Resample(in, int(rateIn), int(rateOut)))

	fundamental := goertzel(out, tone, rateOut)
	image := goertzel(out, imageAt, rateOut)

	if got := db(image / fundamental); got > floorDB {
		t.Errorf("image at %.0f Hz is %.1f dB below the fundamental, want below %.0f dB — "+
			"the filter is not band-limiting", imageAt, got, floorDB)
	}
}

// Downsampling must filter before it decimates, or content above the new
// Nyquist folds back into the speech band as a phantom tone.
func TestResampleDownsamplingSuppressesAliasing(t *testing.T) {
	const (
		tone    = 6000.0 // above the 4 kHz Nyquist of the output rate
		rateIn  = 16000.0
		rateOut = 8000.0
		aliasAt = rateOut - (tone - rateOut) // folds to 2000 Hz
		floorDB = -40.0
	)

	in := sine(tone, rateIn, 16000, 0.5)
	out := interior(Resample(in, int(rateIn), int(rateOut)))

	alias := goertzel(out, aliasAt, rateOut)
	if got := db(alias / 0.5); got > floorDB {
		t.Errorf("a %.0f Hz tone aliased to %.0f Hz at %.1f dB, want below %.0f dB",
			tone, aliasAt, got, floorDB)
	}
}

func TestResampleOutputLength(t *testing.T) {
	for _, c := range []struct {
		from, to, in, want int
	}{
		{8000, 16000, 800, 1600},
		{16000, 8000, 1600, 800},
		{44100, 16000, 44100, 16000},
		{8000, 16000, 0, 0},
	} {
		if got := len(Resample(make([]float32, c.in), c.from, c.to)); got != c.want {
			t.Errorf("%d→%d Hz on %d samples: got %d, want %d", c.from, c.to, c.in, got, c.want)
		}
	}
}

func TestResampleSameRateIsIdentity(t *testing.T) {
	in := sine(440, 16000, 100, 0.3)
	out := Resample(in, 16000, 16000)
	if &out[0] != &in[0] {
		t.Error("same-rate resampling copied the buffer instead of returning it unchanged")
	}
}

func TestResamplerIsReusable(t *testing.T) {
	// One resampler serves every request at a rate pair, so repeated use must
	// not accumulate state.
	r := NewResampler(8000, 16000)
	in := sine(1000, 8000, 4000, 0.5)

	first := r.Resample(in)
	second := r.Resample(in)

	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("sample %d differs between runs: %.6f vs %.6f", i, first[i], second[i])
		}
	}
}

func BenchmarkResample8kTo16k(b *testing.B) {
	// One minute of telephony, the unit the pipeline actually handles.
	in := sine(1000, 8000, 8000*60, 0.5)
	r := NewResampler(8000, 16000)
	b.SetBytes(int64(len(in) * 4))
	b.ResetTimer()
	for b.Loop() {
		_ = r.Resample(in)
	}
}
