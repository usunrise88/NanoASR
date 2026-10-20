package isolation

import (
	"encoding/binary"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/usunrise88/nanoasr/internal/core"
)

// A track keeps one voice and turns the others down, not off: what is left of
// them is quiet, but the recogniser hears quiet speech as well as loud, and
// writes it down. In the transcript by speaker that is a line for somebody who
// said nothing, at the moment somebody else said it — marked as speaking at
// once.
//
// So every word is held against the recording: in its own voice's track a
// word that voice said is about as loud as in the recording, and one leaking
// from another voice is far quieter. Measured on a 38-minute three-voice
// meeting, the words fell in two groups with a gap between: what the voice
// said within 15 dB of the recording, what leaked 33 to 51 dB below it, and
// almost nothing from 21 to 30 dB below. Three in four of the quiet ones were
// literally another track's word at the same moment; the rest were the same
// words misheard. The floor sits in the gap, at 25 dB.
//
// Held against the recording, not an absolute level: a quiet voice is quiet
// in the recording too, and a fixed floor would take it all.

// DefaultLeakFloorDB is how much quieter than the recording a word may be in
// its own track and still be that voice's.
const DefaultLeakFloorDB = -25

// minWord is the shortest stretch a level is measured over: a word's timing
// can be a few milliseconds long, and its level then is noise.
const minWord = 0.08

// silent is the level below which the recording itself says nothing, and a
// comparison with it would mean nothing either.
const silent = -70

// level is the power of 16-bit PCM between two times, in dBFS.
type level struct{ f *os.File }

func (l level) dB(start, end float64) (float64, error) {
	end = max(end, start+minWord)
	a := int64(math.Max(0, start) * SampleRate)
	b := int64(end * SampleRate)
	if b <= a {
		return math.Inf(-1), nil
	}
	buf := make([]byte, 2*(b-a))
	n, err := l.f.ReadAt(buf, 2*a)
	if err != nil && err != io.EOF {
		return 0, err
	}
	n -= n % 2
	if n == 0 {
		return math.Inf(-1), nil
	}
	var sum float64
	for i := 0; i < n; i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(buf[i:]))) / 32768
		sum += v * v
	}
	return 10 * math.Log10(sum/float64(n/2)+1e-12), nil
}

// dropLeaks leaves out of a track's transcript what is far quieter in the
// track than in the recording, and reports how many words it left out.
// floorDB is negative; zero keeps everything.
func dropLeaks(res *core.Result, trackPath, sourcePath string, floorDB float64) (int, error) {
	if floorDB >= 0 || res == nil {
		return 0, nil
	}
	track, err := os.Open(trackPath)
	if err != nil {
		return 0, core.Errorf(core.CodeInternal, "cannot read a track").WithCause(err)
	}
	defer func() { _ = track.Close() }()
	source, err := os.Open(sourcePath)
	if err != nil {
		return 0, core.Errorf(core.CodeInternal, "cannot read the recording").WithCause(err)
	}
	defer func() { _ = source.Close() }()
	own, rec := level{track}, level{source}

	var failed error
	leaked := func(start, end float64) bool {
		r, err := rec.dB(start, end)
		if err != nil || r < silent {
			failed = cmpErr(failed, err)
			return false
		}
		o, err := own.dB(start, end)
		if err != nil {
			failed = cmpErr(failed, err)
			return false
		}
		return o-r < floorDB
	}
	n := prune(res, func(_ int, seg core.Segment, i int) bool {
		if i < 0 {
			// Only the segment's timing is known: it is judged whole.
			return leaked(seg.Start, seg.End)
		}
		return leaked(seg.Words[i].Start, seg.Words[i].End)
	})
	if failed != nil {
		return 0, core.Errorf(core.CodeInternal, "cannot measure a track").WithCause(failed)
	}
	return n, nil
}

func cmpErr(old, err error) error {
	if old != nil {
		return old
	}
	return err
}

// prune drops what drop says from a transcript and counts the words dropped.
// drop is asked about every word of a segment — the segment's index in the
// transcript as it was, the segment, the word's index in it — or, with -1 for
// the word, about a segment that has no word timings. A segment with nothing left goes; one
// that lost words has its text and span written again from the words that
// stay.
func prune(res *core.Result, drop func(s int, seg core.Segment, i int) bool) int {
	dropped := 0
	segments := res.Segments[:0]
	for s, seg := range res.Segments {
		if len(seg.Words) == 0 {
			if drop(s, seg, -1) {
				dropped += len(strings.Fields(seg.Text))
				continue
			}
			segments = append(segments, seg)
			continue
		}
		kept := seg.Words[:0:0]
		for i, w := range seg.Words {
			if drop(s, seg, i) {
				dropped++
				continue
			}
			kept = append(kept, w)
		}
		if len(kept) == 0 {
			continue
		}
		if len(kept) < len(seg.Words) {
			texts := make([]string, len(kept))
			for i, w := range kept {
				texts[i] = w.Word
			}
			seg.Text = strings.Join(texts, " ")
			seg.Start, seg.End = kept[0].Start, kept[len(kept)-1].End
			seg.Words = kept
		}
		segments = append(segments, seg)
	}
	res.Segments = segments
	return dropped
}

// A voice can also pass into another's track at full strength, where the
// extractor took the wrong voice for a while: then the same words come out of
// both tracks at the same moment, and a level against the recording cannot
// tell which is the copy. Two people saying the same words at the same
// instant is rare, though, and of two copies the one in the louder track is
// the voice's own. So words matched across tracks are grouped into runs, and
// the track that holds a run more quietly loses it whole — whole, so a phrase
// is not split between two speakers word by word. A single shared word goes
// only when one copy is clearly the louder: two people saying "yes" at once
// is the one coincidence common enough to keep.

const (
	// echoWindow is how far apart in time the two copies of a word may start.
	echoWindow = 0.5
	// runGap is the longest step from one word of a run to the next: words
	// further apart are separate utterances, however their numbers follow.
	runGap = 1.0
	// echoMarginDB is how much louder one copy of a single shared word must
	// be for the other to go.
	echoMarginDB = 3
)

type echoWord struct {
	part, seg, word int
	// seq is the word's place in its track's transcript.
	seq   int
	start float64
	text  string
	db    float64
}

// dropEchoes removes from the parts the copies of words another track holds
// louder, and reports how many words it removed.
func dropEchoes(parts []part, paths map[string]string) (int, error) {
	var words []echoWord
	for p, pt := range parts {
		f, err := os.Open(paths[pt.speaker])
		if err != nil {
			return 0, core.Errorf(core.CodeInternal, "cannot read a track").WithCause(err)
		}
		lv := level{f}
		seq := 0
		for s, seg := range pt.result.Segments {
			for w, wd := range seg.Words {
				text := wordKey(wd.Word)
				db, err := lv.dB(wd.Start, wd.End)
				if err != nil {
					_ = f.Close()
					return 0, core.Errorf(core.CodeInternal, "cannot measure a track").WithCause(err)
				}
				if text != "" {
					words = append(words, echoWord{part: p, seg: s, word: w, seq: seq, start: wd.Start, text: text, db: db})
				}
				seq++
			}
		}
		_ = f.Close()
	}
	sort.Slice(words, func(i, j int) bool { return words[i].start < words[j].start })

	// Pairs of copies, by the pair of tracks they are in.
	type pair struct{ a, b int }
	byTracks := map[[2]int][]pair{}
	for i := range words {
		for j := i + 1; j < len(words) && words[j].start-words[i].start < echoWindow; j++ {
			a, b := i, j
			if words[a].part == words[b].part || words[a].text != words[b].text {
				continue
			}
			if words[a].part > words[b].part {
				a, b = b, a
			}
			k := [2]int{words[a].part, words[b].part}
			byTracks[k] = append(byTracks[k], pair{a, b})
		}
	}

	drop := map[[3]int]bool{}
	for _, pairs := range byTracks {
		sort.Slice(pairs, func(i, j int) bool { return words[pairs[i].a].seq < words[pairs[j].a].seq })
		// A run is pairs whose words follow one another in both tracks, and
		// closely in time.
		for i := 0; i < len(pairs); {
			j := i + 1
			for j < len(pairs) &&
				words[pairs[j].a].seq-words[pairs[j-1].a].seq <= 1 &&
				words[pairs[j].b].seq-words[pairs[j-1].b].seq <= 1 &&
				words[pairs[j].a].start-words[pairs[j-1].a].start <= runGap {
				j++
			}
			run := pairs[i:j]
			var la, lb float64
			for _, pr := range run {
				la += words[pr.a].db
				lb += words[pr.b].db
			}
			la, lb = la/float64(len(run)), lb/float64(len(run))
			if len(run) > 1 || math.Abs(la-lb) >= echoMarginDB {
				for _, pr := range run {
					q := words[pr.a]
					if la > lb {
						q = words[pr.b]
					}
					drop[[3]int{q.part, q.seg, q.word}] = true
				}
			}
			i = j
		}
	}

	dropped := 0
	for p := range parts {
		dropped += prune(parts[p].result, func(s int, _ core.Segment, w int) bool {
			return w >= 0 && drop[[3]int{p, s, w}]
		})
	}
	return dropped, nil
}

// wordKey is a word as it is compared across tracks: its letters and
// digits, in lower case.
func wordKey(w string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, w)
}
