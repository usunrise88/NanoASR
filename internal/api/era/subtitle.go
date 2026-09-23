package era

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/usunrise88/nanoasr/internal/core"
)

// This file reproduces whisperx's SubtitlesWriter, which is what the reference
// service renders srt and vtt through.
//
// It is not the one-cue-per-segment rendering the shared subtitle package does,
// and the difference is not cosmetic: with the settings below the writer
// ignores segment boundaries entirely, pours every word into one cue and breaks
// only where the speaker paused. A client that draws subtitles sees a
// completely different cut if we hand it per-segment cues instead.
//
// The three settings are the service's environment defaults —
// SUBTITLE_MAX_LINE_WIDTH, SUBTITLE_MAX_LINE_COUNT and
// SUBTITLE_HIGHLIGHT_WORDS. A deployment that overrides them renders
// differently, and there is nothing in the wire protocol that would let us find
// out which values are in effect, so the defaults are what we reproduce.
const (
	maxLineWidth = 1000
	maxLineCount = 2

	// longPause is the gap between two consecutive word onsets that starts a
	// new cue. With a line width of 1000 characters it is, in practice, the
	// only thing that ever does.
	longPause = 3.0
)

// preserveSegments is what whisperx computes as
// `max_line_count is None or raw_max_line_width is None`. Both settings have
// values, so it is false: segment boundaries do not break a cue, and the
// writer's seg_break branch is unreachable. It is spelled out here because a
// reader comparing this file to the original will look for that branch.
const preserveSegments = false

// cue is one rendered subtitle: a span and the text inside it.
type cue struct {
	start float64
	end   float64
	text  string
}

// cueWord is one word on its way into a cue, carrying the segment it came from
// because the speaker label is a property of the segment.
type cueWord struct {
	word    string
	start   float64
	end     float64
	speaker *string
}

// cues renders a result the way whisperx's SubtitlesWriter.iterate_result does.
func cues(res *core.Result) []cue {
	if len(res.Segments) == 0 {
		return nil
	}
	// whisperx tests the first segment only, and takes the segment-level path
	// for the whole result when it has no words. Ours always has words unless
	// the recognition was empty, but the fallback is reproduced rather than
	// assumed away.
	if len(res.Segments[0].Words) == 0 {
		return segmentCues(res)
	}
	return wordCues(res)
}

// segmentCues is the path whisperx takes for a result without word timings:
// one cue per segment, with the arrow sequence defused so it cannot be read as
// a cue separator.
func segmentCues(res *core.Result) []cue {
	out := make([]cue, 0, len(res.Segments))
	for _, s := range res.Segments {
		text := strings.ReplaceAll(strings.TrimSpace(s.Text), "-->", "->")
		out = append(out, cue{start: s.Start, end: s.End, text: withSpeaker(text, s.Speaker)})
	}
	return out
}

// wordCues groups words into cues and turns each group into one.
func wordCues(res *core.Result) []cue {
	groups := groupWords(res)
	out := make([]cue, 0, len(groups))
	for _, g := range groups {
		out = append(out, renderCue(g, res.Language))
	}
	return out
}

// groupWords is the accumulator loop of iterate_subtitles.
//
// lineLen and lineCount track the cue being built: a word that does not fit the
// line either wraps onto the next one or, once the cue already holds
// maxLineCount lines or the speaker paused, starts a new cue.
func groupWords(res *core.Result) [][]cueWord {
	var (
		groups  [][]cueWord
		current []cueWord
		lineLen int
	)
	lineCount := 1
	last := res.Segments[0].Start

	for _, s := range res.Segments {
		for _, word := range s.Words {
			w := cueWord{word: word.Word, start: word.Start, end: word.End, speaker: s.Speaker}
			// whisperx measures the gap between word onsets, not from the end
			// of the previous word to the start of this one.
			pause := !preserveSegments && w.start-last > longPause
			hasRoom := lineLen+utf8.RuneCountInString(w.word) <= maxLineWidth

			if lineLen > 0 && hasRoom && !pause {
				lineLen += utf8.RuneCountInString(w.word)
			} else {
				w.word = strings.TrimSpace(w.word)
				switch {
				case len(current) > 0 && (pause || lineCount >= maxLineCount):
					groups = append(groups, current)
					current = nil
					lineCount = 1
				case lineLen > 0:
					lineCount++
					w.word = "\n" + w.word
				}
				lineLen = utf8.RuneCountInString(strings.TrimSpace(w.word))
			}

			current = append(current, w)
			last = w.start
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// renderCue turns one group of words into a cue.
//
// The span is the widest the words cover, not the enclosing segment's: whisperx
// takes min(start) and max(end) over the group, and a cue that spans two
// segments has no single segment to borrow a span from anyway.
func renderCue(g []cueWord, language string) cue {
	start, end := g[0].start, g[0].end
	parts := make([]string, 0, len(g))
	for _, w := range g {
		start = math.Min(start, w.start)
		end = math.Max(end, w.end)
		parts = append(parts, w.word)
	}
	sep := " "
	if noSpaceLanguage(language) {
		sep = ""
	}
	return cue{start: start, end: end, text: withSpeaker(strings.Join(parts, sep), g[0].speaker)}
}

// noSpaceLanguage is whisperx's LANGUAGES_WITHOUT_SPACES: the two it writes
// without separating the words.
func noSpaceLanguage(language string) bool {
	return language == "ja" || language == "zh"
}

// withSpeaker prefixes the label whisperx uses when diarization ran. The
// reference service ships with diarization off, so this is additive: a client
// of it has never seen the prefix and only sees it if it asks for diarize=true.
func withSpeaker(text string, speaker *string) string {
	if speaker == nil {
		return text
	}
	return "[" + *speaker + "]: " + text
}

// srt renders SubRip: numbered cues, hours always present, comma before the
// milliseconds.
func srt(res *core.Result) string {
	var b strings.Builder
	for i, c := range cues(res) {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
			i+1, timecode(c.start, true, ','), timecode(c.end, true, ','), c.text)
	}
	return b.String()
}

// vtt renders WebVTT, where the hours are omitted below the first one.
//
// Not <v speaker> as the native dialect writes it: this contract's clients
// parse whisperx's "[spk]: " prefix, and a voice span they have never seen
// would be displayed verbatim by a player that does not know the tag.
func vtt(res *core.Result) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues(res) {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n",
			timecode(c.start, false, '.'), timecode(c.end, false, '.'), c.text)
	}
	return b.String()
}

// timecode is whisper's format_timestamp: milliseconds rounded rather than
// truncated, and the hours field written only when it is asked for or needed.
func timecode(seconds float64, includeHours bool, marker rune) string {
	if seconds < 0 {
		// The reference asserts non-negative and crashes; a transcript is not
		// worth a 500.
		seconds = 0
	}
	ms := int64(math.RoundToEven(seconds * 1000))
	h := ms / 3_600_000
	ms -= h * 3_600_000
	m := ms / 60_000
	ms -= m * 60_000
	s := ms / 1_000
	ms -= s * 1_000

	if includeHours || h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d%c%03d", h, m, s, marker, ms)
	}
	return fmt.Sprintf("%02d:%02d%c%03d", m, s, marker, ms)
}
