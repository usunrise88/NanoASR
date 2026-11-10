package realtime

import (
	"math"
	"slices"
	"time"
)

// A session's audio is measured as it arrives so that the engine can tell
// whether the last moments were quiet. That one question is what makes an
// utterance boundary safe.
//
// The recogniser's own endpointer decides from tokens it has decoded, and a
// streaming model decodes late: a word that has been spoken but not yet
// emitted looks, to the endpointer, exactly like silence. Cutting the
// utterance there throws the word away, because the stream is reset and the
// audio it was made of has already been consumed. The meter looks at the audio
// instead of the tokens, so it is not behind.
//
// It is deliberately the simplest thing that can work: loudness in 10 ms bins
// against a noise floor learned from the session's own recent past. No model,
// nothing to load, and the same answer for the same samples.

const (
	// binDuration is the resolution of the loudness series.
	binDuration = 10 * time.Millisecond
	// meterKeep is how much of the series is kept: enough to learn a noise
	// floor from, which needs a pause or two in it.
	meterKeep = 30 * time.Second
	// floorBlock is the length of the blocks the noise floor is picked from.
	floorBlock = 50 * time.Millisecond
	// floorSpan is how many blocks are averaged before the quietest stretch is
	// picked as the noise floor: 150 ms, the shortest pause that is worth
	// calling one. Taking the quietest single block instead would let one
	// dropped packet — a block of exact zeros in the middle of a call — set a
	// floor that nothing real is near; averaged with its neighbours it moves
	// the floor by a few dB.
	floorSpan = 3
	// speechMarginDB is how far above the noise floor audio has to be to count
	// as something other than quiet.
	speechMarginDB = 12.0
	// levelPercentile and levelWindow say what speech level is: the loudness
	// that 5% of the 50 ms blocks of the last ten seconds exceed.
	levelPercentile = 0.95
	levelWindow     = 10 * time.Second
	// speechContrastDB is how far above the floor the level has to be before it
	// is believed to be speech and not just the noise being a little louder.
	speechContrastDB = 20.0
	// belowSpeechDB is how far under the speech level quiet is, when there is
	// speech to measure against. It is what stops a session with no pause in
	// the last half minute from learning a floor out of the gaps between words:
	// those are 15 dB under the speech at the very most, and a pause is much
	// more.
	belowSpeechDB = 15.0
	// floorMinDB bounds the floor from below. A recording padded with exact
	// zeros would otherwise put it at -120 dB.
	floorMinDB = -75.0
	// floorMaxDB is the loudest a noise floor is believed to be. Above it, the
	// quietest stretch of the last half minute is not a pause but a quiet
	// passage of speech, or a line too noisy to tell from speech, and the meter
	// says it does not know rather than call speech quiet.
	floorMaxDB = -40.0
	// minHistory is how much audio the meter wants to have heard before it
	// claims to know what quiet is.
	minHistory = time.Second
	// floorRefresh is how often the floor is recomputed. It moves slowly, and
	// sorting thirty seconds of blocks on every 20 ms frame would be waste.
	floorRefresh = 500 * time.Millisecond
)

// meter is the loudness of one session's audio, in 10 ms bins of mean power.
// Loop goroutine only.
type meter struct {
	rate   int
	perBin int
	acc    float64
	n      int

	// power holds the closed bins, oldest first, trimmed to meterKeep.
	power []float64

	limitDB    float64
	limitOK    bool
	limitAt    int // len of the series when the limit was last computed, in bins
	limitValid bool
}

// add measures samples, which are at the given rate. A change of rate drops
// the half-filled bin rather than mixing two clocks in it.
func (m *meter) add(rate int, samples []float32) {
	if rate <= 0 {
		return
	}
	if rate != m.rate {
		m.rate = rate
		m.perBin = max(1, rate*int(binDuration/time.Millisecond)/1000)
		m.acc, m.n = 0, 0
	}
	for _, s := range samples {
		m.acc += float64(s) * float64(s)
		m.n++
		if m.n == m.perBin {
			m.power = append(m.power, m.acc/float64(m.n))
			m.acc, m.n = 0, 0
		}
	}
	if keep := int(meterKeep / binDuration); len(m.power) > 2*keep {
		// Trimmed in one go now and then instead of one bin at a time, so the
		// cost is amortised and the slice is not shifted on every frame.
		m.power = append(m.power[:0], m.power[len(m.power)-keep:]...)
		m.limitAt = 0
	}
}

func db(power float64) float64 { return 10 * math.Log10(power+1e-12) }

// limit is the loudness, in dB, below which audio counts as quiet. ok is false
// when the meter cannot tell: there is too little audio, or nothing in the last
// half minute was quiet enough to be a noise floor.
//
// It is the noise floor plus a margin — the floor being the quietest 150 ms of
// the recent past, which is the line when nobody is speaking — and, when there
// is speech to compare with, no higher than 15 dB under that speech. The second
// condition is for the stretch where the quietest 150 ms is the gap between two
// words, and the limit would otherwise sit among them.
func (m *meter) limit() (float64, bool) {
	if time.Duration(len(m.power))*binDuration < minHistory {
		return 0, false
	}
	if m.limitValid && time.Duration(len(m.power)-m.limitAt)*binDuration < floorRefresh {
		return m.limitDB, m.limitOK
	}

	m.limitAt, m.limitValid = len(m.power), true
	m.limitDB, m.limitOK = 0, false

	blocks := m.blocks(meterKeep)
	if len(blocks) < floorSpan {
		return 0, false
	}
	floor := math.Inf(1)
	for i := 0; i+floorSpan <= len(blocks); i++ {
		sum := 0.0
		for _, p := range blocks[i : i+floorSpan] {
			sum += p
		}
		floor = min(floor, db(sum/floorSpan))
	}
	floor = max(floor, floorMinDB)
	if floor > floorMaxDB {
		return 0, false
	}

	limit := floor + speechMarginDB
	recent := blocks[max(0, len(blocks)-int(levelWindow/floorBlock)):]
	levels := make([]float64, len(recent))
	for i, p := range recent {
		levels[i] = db(p)
	}
	if level := percentile(levels, levelPercentile); level >= floor+speechContrastDB {
		limit = min(limit, level-belowSpeechDB)
	}

	m.limitDB, m.limitOK = limit, true
	return limit, true
}

// blocks is the mean power of the last d of audio, in 50 ms blocks.
func (m *meter) blocks(d time.Duration) []float64 {
	per := int(floorBlock / binDuration)
	start := max(0, len(m.power)-int(d/binDuration))
	out := make([]float64, 0, (len(m.power)-start)/per)
	for i := start; i+per <= len(m.power); i += per {
		sum := 0.0
		for _, p := range m.power[i : i+per] {
			sum += p
		}
		out = append(out, sum/float64(per))
	}
	return out
}

// percentile of values, which it sorts a copy of.
func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return math.Inf(-1)
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted[int(float64(len(sorted)-1)*q)]
}

// quietFor is how long the audio has been quiet, counting back from its end,
// and not counting past atMost: callers compare it with a threshold, and
// stopping there keeps the cost of asking on every piece of audio small however
// long the silence has been. ok is false when the meter has not heard enough to
// judge, in which case the caller should fall back on whatever it would have
// done without a meter.
//
// A bin is judged on itself and its neighbours, so one stray bin of noise does
// not flicker the answer. A click inside a pause does restart the count, though:
// that errs towards "not quiet yet", which only ever delays a boundary.
func (m *meter) quietFor(atMost time.Duration) (quiet time.Duration, ok bool) {
	limit, ok := m.limit()
	if !ok {
		return 0, false
	}

	// Rounded up: a cap below the threshold the caller compares with would make
	// the threshold unreachable.
	cap := int((atMost + binDuration - 1) / binDuration)
	bins := 0
	for i := len(m.power) - 1; i >= 0 && bins < cap; i-- {
		if db(m.smoothed(i)) >= limit {
			break
		}
		bins++
	}
	return time.Duration(bins) * binDuration, true
}

// smoothed is the mean power of bin i and the bins either side of it, or of as
// many of them as exist at the ends of the series.
func (m *meter) smoothed(i int) float64 {
	lo, hi := max(0, i-1), min(len(m.power)-1, i+1)
	sum := 0.0
	for _, p := range m.power[lo : hi+1] {
		sum += p
	}
	return sum / float64(hi-lo+1)
}
