package diarize

import (
	"sort"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Absorption thresholds for Split.
//
// These are the whole difference between a usable transcript and confetti. A
// diarizer will occasionally attribute one word in the middle of a sentence to
// the other speaker, and splitting on it would turn one segment into three, two
// of which are a single word. A run has to be long enough and wordy enough to
// be a turn before it is allowed to break a sentence apart.
const (
	MinTurnSec   = 0.5
	MinTurnWords = 2
)

// Apply attaches attributions to the words of every segment, in the order
// Result.Words() flattens them.
//
// It returns false when the counts disagree, which would mean the words were
// rewritten between diarization and here — better to leave the transcript
// unattributed than to label it from a stale mapping.
func Apply(segs []core.Segment, attrs []Attribution) bool {
	n := 0
	for _, s := range segs {
		n += len(s.Words)
	}
	if n != len(attrs) {
		return false
	}
	i := 0
	for si := range segs {
		for wi := range segs[si].Words {
			a := attrs[i]
			i++
			if a.Speaker == "" {
				continue
			}
			speaker := a.Speaker
			segs[si].Words[wi].Speaker = &speaker
			segs[si].Words[wi].SpeakerConfidence = a.Confidence
		}
	}
	return true
}

// Split cuts each segment where the speaker changes, so that a segment never
// claims a speaker its own words contradict.
//
// SPEC §5.7 attributes per word and SPEC §6's schema carries a speaker per
// segment. Splitting is what makes both true at once: the alternative is a
// majority vote presented as a fact.
func Split(segs []core.Segment) []core.Segment {
	out := make([]core.Segment, 0, len(segs))
	for _, s := range segs {
		out = append(out, splitOne(s)...)
	}
	for i := range out {
		out[i].ID = i
	}
	return out
}

func splitOne(s core.Segment) []core.Segment {
	if len(s.Words) == 0 {
		return []core.Segment{s}
	}
	runs := speakerRuns(s.Words)
	if len(runs) <= 1 {
		// One speaker for the whole segment: keep it whole, and let it say so.
		s.Speaker = dominantSpeaker(s.Words)
		return []core.Segment{s}
	}

	out := make([]core.Segment, 0, len(runs))
	for i, r := range runs {
		words := s.Words[r.from:r.to]
		child := core.Segment{
			Text:    wordsText(words),
			Channel: s.Channel,
			Speaker: dominantSpeaker(words),
			Words:   words,
		}
		// The outer edges belong to the parent: the first child starts where
		// the segment did, not where its first word did, or the transcript
		// would drift away from the audio it was cut from.
		child.Start = words[0].Start
		child.End = words[len(words)-1].End
		if i == 0 {
			child.Start = s.Start
		}
		if i == len(runs)-1 {
			child.End = s.End
		}
		child.AvgConfidence = averageConfidence(words)
		out = append(out, child)
	}
	return out
}

type run struct{ from, to int }

// speakerRuns groups consecutive words by speaker, absorbing runs too short to
// be a turn into the run before them.
func speakerRuns(words []core.Word) []run {
	var runs []run
	for i := range words {
		if i > 0 && sameSpeaker(words[i-1], words[i]) {
			runs[len(runs)-1].to = i + 1
			continue
		}
		runs = append(runs, run{from: i, to: i + 1})
	}

	// Absorb and re-join in one pass. Absorbing alone is not enough: a short
	// run swallowed by the run before it leaves the run after it stranded, and
	// the two would then be adjacent segments claiming the same speaker.
	out := runs[:0]
	for _, r := range runs {
		if len(out) == 0 {
			out = append(out, r)
			continue
		}
		last := &out[len(out)-1]
		if !isTurn(words[r.from:r.to]) || sameSpeaker(words[last.from], words[r.from]) {
			last.to = r.to
			continue
		}
		out = append(out, r)
	}

	// The first run has no run before it to be absorbed into, so a single
	// misattributed opening word survives the loop above and becomes a segment
	// of its own — observed as a 0.1-second spk_0 in front of a sentence that
	// belongs entirely to spk_1. It joins the run that follows instead, which
	// then claims the speaker holding most of its time.
	if len(out) > 1 && !isTurn(words[out[0].from:out[0].to]) {
		out[1].from = out[0].from
		out = out[1:]
	}
	return out
}

// isTurn reports whether a run is substantial enough to break a segment apart.
func isTurn(words []core.Word) bool {
	return len(words) >= MinTurnWords &&
		words[len(words)-1].End-words[0].Start >= MinTurnSec
}

func sameSpeaker(a, b core.Word) bool {
	if a.Speaker == nil || b.Speaker == nil {
		return a.Speaker == b.Speaker
	}
	return *a.Speaker == *b.Speaker
}

// dominantSpeaker reports the speaker a segment should claim: the one holding
// most of its spoken time.
//
// Not the first word's speaker. Once a run can absorb a neighbour too short to
// be a turn of its own, the first word is exactly the one likely to be the
// absorbed mistake, and letting it name the segment would hand the whole
// sentence to the wrong person.
//
// The returned pointer is a copy, because two segments must not share one.
func dominantSpeaker(ws []core.Word) *string {
	byID := map[string]float64{}
	for _, w := range ws {
		if w.Speaker == nil {
			continue
		}
		d := w.End - w.Start
		if d <= 0 {
			d = 0 // a zero-length word still counts as a vote, not as time
		}
		byID[*w.Speaker] += d + tieBreak
	}
	if len(byID) == 0 {
		return nil
	}
	best, bestTime := "", -1.0
	for id, t := range byID {
		// Sorted by id on a tie so that the same input always names the same
		// speaker: map iteration order would otherwise make the result differ
		// between runs of the same file.
		if t > bestTime || (t == bestTime && id < best) {
			best, bestTime = id, t
		}
	}
	s := best
	return &s
}

// tieBreak is the weight of a word beyond its duration, so that a run of many
// very short words is not outvoted by one long one.
const tieBreak = 0.01

// Speakers summarises the clusters that actually reached the transcript.
//
// TotalSpeech is measured over segments rather than raw turns: a turn the
// recogniser produced no words for is not speech anyone can see, and reporting
// it would make the summary disagree with the transcript beside it.
func Speakers(segs []core.Segment) []core.Speaker {
	type acc struct {
		speech   float64
		segments int
	}
	byID := map[string]*acc{}
	for _, s := range segs {
		if s.Speaker == nil {
			continue
		}
		a := byID[*s.Speaker]
		if a == nil {
			a = &acc{}
			byID[*s.Speaker] = a
		}
		a.speech += s.End - s.Start
		a.segments++
	}
	if len(byID) == 0 {
		return nil
	}

	out := make([]core.Speaker, 0, len(byID))
	for id, a := range byID {
		out = append(out, core.Speaker{ID: id, TotalSpeech: a.speech, Segments: a.segments})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func wordsText(ws []core.Word) string {
	out := ""
	for i, w := range ws {
		if i > 0 {
			out += " "
		}
		out += w.Word
	}
	return out
}

func averageConfidence(ws []core.Word) float64 {
	sum, n := 0.0, 0
	for _, w := range ws {
		if w.Confidence > 0 {
			sum += w.Confidence
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
