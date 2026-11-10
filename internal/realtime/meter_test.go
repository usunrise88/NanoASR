package realtime

import (
	"math"
	"testing"
	"time"
)

// tone is a deterministic stand-in for audio at a given level: a sum of two
// sines, so it has no silent samples, at amplitude amp.
func tone(rate int, d time.Duration, amp float64) []float32 {
	n := int(float64(rate) * d.Seconds())
	out := make([]float32, n)
	for i := range out {
		t := float64(i) / float64(rate)
		out[i] = float32(amp * (0.6*math.Sin(2*math.Pi*220*t) + 0.4*math.Sin(2*math.Pi*1370*t)))
	}
	return out
}

const (
	loud  = 0.2    // about -17 dBFS
	hiss  = 0.0004 // about -68 dBFS, a quiet line
	rate  = 16000
	frame = 20 * time.Millisecond
)

// feedMeter adds audio the way the engine does, in 20 ms frames.
func feedMeter(m *meter, samples []float32) {
	step := rate * int(frame/time.Millisecond) / 1000
	for off := 0; off < len(samples); off += step {
		m.add(rate, samples[off:min(off+step, len(samples))])
	}
}

func near(t *testing.T, got, want, tol time.Duration, what string) {
	t.Helper()
	if got < want-tol || got > want+tol {
		t.Errorf("%s: got %s, want %s ± %s", what, got, want, tol)
	}
}

func TestMeterNeedsHistoryBeforeJudging(t *testing.T) {
	var m meter
	feedMeter(&m, tone(rate, 300*time.Millisecond, hiss))
	if _, ok := m.quietFor(time.Minute); ok {
		t.Fatal("judged after 300 ms of audio; it has nothing to learn a floor from")
	}
}

func TestMeterMeasuresTheTrailingPause(t *testing.T) {
	var m meter
	feedMeter(&m, tone(rate, 1500*time.Millisecond, hiss)) // the line before anyone speaks
	feedMeter(&m, tone(rate, 3*time.Second, loud))
	feedMeter(&m, tone(rate, 400*time.Millisecond, hiss))

	got, ok := m.quietFor(time.Minute)
	if !ok {
		t.Fatal("not ready after five seconds of audio")
	}
	near(t, got, 400*time.Millisecond, 30*time.Millisecond, "the pause after speech")
}

func TestMeterSaysZeroWhileSpeechIsOngoing(t *testing.T) {
	var m meter
	feedMeter(&m, tone(rate, 1500*time.Millisecond, hiss))
	feedMeter(&m, tone(rate, 3*time.Second, loud))
	got, ok := m.quietFor(time.Minute)
	if !ok || got != 0 {
		t.Fatalf("quietFor = %s, %v while the speaker is talking, want 0", got, ok)
	}
}

func TestMeterCountsAPauseFromTheLastClickInIt(t *testing.T) {
	var m meter
	feedMeter(&m, tone(rate, 1500*time.Millisecond, hiss))
	feedMeter(&m, tone(rate, 2*time.Second, loud))
	feedMeter(&m, tone(rate, 300*time.Millisecond, hiss))
	// One bin of noise in the middle of the quiet: a pop on the line.
	feedMeter(&m, tone(rate, 10*time.Millisecond, loud))
	feedMeter(&m, tone(rate, 300*time.Millisecond, hiss))

	// The pop is not mistaken for the end of the whole pause by a stray bin, but
	// it does restart the count, which can only delay a boundary and never move
	// one into speech.
	got, _ := m.quietFor(time.Minute)
	near(t, got, 300*time.Millisecond, 40*time.Millisecond, "the quiet since the pop")
}

func TestMeterLearnsTheFloorFromTheLine(t *testing.T) {
	// The same speech over a noisier line: the pause is not as quiet in
	// absolute terms, and still has to be recognised as one.
	const noisy = 0.004 // about -48 dBFS
	var m meter
	feedMeter(&m, tone(rate, 1500*time.Millisecond, noisy))
	feedMeter(&m, tone(rate, 3*time.Second, loud))
	feedMeter(&m, tone(rate, 500*time.Millisecond, noisy))

	got, ok := m.quietFor(time.Minute)
	if !ok {
		t.Fatal("not ready")
	}
	near(t, got, 500*time.Millisecond, 30*time.Millisecond, "the pause on a noisy line")
}

func TestMeterOnlyFindsShortDipsInContinuousSpeech(t *testing.T) {
	// No pause anywhere: whatever the meter calls quiet here must be shorter
	// than any pause the engine will cut at, or a word would be cut in half.
	var m meter
	var longest time.Duration
	for i := 0; i < 20; i++ {
		feedMeter(&m, tone(rate, 400*time.Millisecond, loud))
		feedMeter(&m, tone(rate, 40*time.Millisecond, loud/6)) // a dip between words
		if got, _ := m.quietFor(time.Minute); got > longest {
			longest = got
		}
	}
	if longest > 60*time.Millisecond {
		t.Fatalf("a 40 ms dip in continuous speech was counted as %s of quiet", longest)
	}
	feedMeter(&m, tone(rate, 200*time.Millisecond, loud))
	if got, _ := m.quietFor(time.Minute); got != 0 {
		t.Fatalf("quietFor = %s while speaking, want 0", got)
	}
}

func TestMeterDropsTheHalfFilledBinOnARateChange(t *testing.T) {
	var m meter
	m.add(16000, make([]float32, 100)) // 100 of 160 samples of a bin
	m.add(8000, make([]float32, 80))   // exactly one 10 ms bin at 8 kHz
	if len(m.power) != 1 {
		t.Fatalf("%d bins after a rate change, want 1: the old half-bin must not be mixed in", len(m.power))
	}
}

func TestMeterKeepsAboutThirtySeconds(t *testing.T) {
	var m meter
	feedMeter(&m, tone(rate, 100*time.Second, hiss))
	if got := time.Duration(len(m.power)) * binDuration; got > 2*meterKeep {
		t.Fatalf("kept %s of bins, want at most %s", got, 2*meterKeep)
	}
	// And the series still ends at the newest audio.
	feedMeter(&m, tone(rate, 2*time.Second, loud))
	if got, _ := m.quietFor(time.Minute); got != 0 {
		t.Fatalf("quietFor = %s after trimming, want 0 while speech is ongoing", got)
	}
}

func TestAQuietThresholdThatIsNotWholeBinsIsReachable(t *testing.T) {
	// Callers ask for the quiet up to the threshold they compare against, and a
	// threshold of 187.5 ms must be reachable by a 190 ms pause; counting stopped
	// at 180 ms once, and a pause that long was never found.
	var m meter
	feedMeter(&m, tone(rate, 1500*time.Millisecond, hiss))
	feedMeter(&m, tone(rate, 3*time.Second, loud))
	feedMeter(&m, tone(rate, 220*time.Millisecond, hiss))

	got, _ := m.quietFor(187500 * time.Microsecond)
	if got < 187500*time.Microsecond {
		t.Fatalf("quietFor(187.5ms) = %s after 220 ms of quiet, want it to reach the threshold", got)
	}
}

func TestAMeterThatHasNeverHeardQuietSaysItCannotTell(t *testing.T) {
	// Speech from the start with nothing that could be a noise floor: the
	// quietest stretch is a quiet word, and calling that silence is how a word
	// gets cut in half.
	var m meter
	feedMeter(&m, tone(rate, 4*time.Second, loud))
	if _, ok := m.quietFor(time.Minute); ok {
		t.Fatal("the meter claimed to know what quiet is after four seconds of speech and nothing else")
	}
}
