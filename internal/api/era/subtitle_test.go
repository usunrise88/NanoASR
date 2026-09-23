package era

import (
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

// twoSegments builds a result whose words are split across two segments, so a
// renderer that honours segment boundaries and one that does not give visibly
// different answers.
func twoSegments(secondStart float64) *core.Result {
	return &core.Result{
		Language: "ru",
		Segments: []core.Segment{
			{
				ID: 0, Start: 0.5, End: 2.04, Text: "Привет, мир",
				Words: []core.Word{
					{Word: "Привет,", Start: 0.5, End: 1.1},
					{Word: "мир", Start: 1.2, End: 2.04},
				},
			},
			{
				ID: 1, Start: secondStart, End: secondStart + 1.0, Text: "как дела",
				Words: []core.Word{
					{Word: "как", Start: secondStart, End: secondStart + 0.3},
					{Word: "дела", Start: secondStart + 0.4, End: secondStart + 1.0},
				},
			},
		},
	}
}

// The reference renders subtitles through whisperx's SubtitlesWriter with
// max_line_count set, which makes preserve_segments false: segment boundaries
// do not break a cue. Two segments a moment apart are one subtitle.
func TestSubtitleCuesIgnoreSegmentBoundaries(t *testing.T) {
	got := vtt(twoSegments(2.2))
	want := "WEBVTT\n\n00:00.500 --> 00:03.200\nПривет, мир как дела\n\n"
	if got != want {
		t.Errorf("vtt =\n%q\nwant\n%q", got, want)
	}
}

// What does break a cue is a pause longer than three seconds between two word
// onsets — with a thousand-character line width it is the only thing that does.
func TestSubtitleCuesBreakOnALongPause(t *testing.T) {
	got := vtt(twoSegments(6.0))
	want := "WEBVTT\n\n00:00.500 --> 00:02.040\nПривет, мир\n\n" +
		"00:06.000 --> 00:07.000\nкак дела\n\n"
	if got != want {
		t.Errorf("vtt =\n%q\nwant\n%q", got, want)
	}
}

// The cue span is the widest the words cover, not the enclosing segment's: a
// cue that crosses a segment has no single segment to borrow a span from.
func TestSubtitleCueSpansTheWordsItHolds(t *testing.T) {
	res := twoSegments(2.2)
	// A segment whose declared bounds are wider than its words must not widen
	// the cue.
	res.Segments[0].Start = 0.0
	res.Segments[1].End = 99.0

	if got, want := srt(res), "1\n00:00:00,500 --> 00:00:03,200\nПривет, мир как дела\n\n"; got != want {
		t.Errorf("srt =\n%q\nwant\n%q", got, want)
	}
}

// SubRip numbers its cues and always writes the hours; WebVTT omits them below
// the first hour, which is where the reference and the native dialect differ.
func TestSubtitleTimecodeFormats(t *testing.T) {
	for _, tc := range []struct {
		seconds      float64
		includeHours bool
		marker       rune
		want         string
	}{
		{0.5, false, '.', "00:00.500"},
		{0.5, true, ',', "00:00:00,500"},
		{3661.007, false, '.', "01:01:01.007"},
		{3661.007, true, ',', "01:01:01,007"},
		// Rounded, not truncated: whisper's format_timestamp rounds, so a
		// timing four ten-thousandths under a second is a whole second.
		{0.9996, false, '.', "00:01.000"},
		{-1, true, ',', "00:00:00,000"},
	} {
		if got := timecode(tc.seconds, tc.includeHours, tc.marker); got != tc.want {
			t.Errorf("timecode(%v, %v) = %q, want %q",
				tc.seconds, tc.includeHours, got, tc.want)
		}
	}
}

// A diarized transcript carries whisperx's "[spk]: " prefix, in the cue and in
// the plain-text rendering alike. The reference ships diarization off, so a
// client of it only ever sees this if it asks for diarize=true.
func TestSubtitlesAndTextLabelTheSpeaker(t *testing.T) {
	speaker := "spk_0"
	res := twoSegments(2.2)
	res.Segments[0].Speaker = &speaker
	res.Segments[1].Speaker = &speaker

	if got := vtt(res); !strings.Contains(got, "[spk_0]: Привет, мир как дела") {
		t.Errorf("vtt lost the speaker label:\n%s", got)
	}
	if got, want := txt(res), "[spk_0]: Привет, мир\n[spk_0]: как дела\n"; got != want {
		t.Errorf("txt = %q, want %q", got, want)
	}
}

// A result whose recognition produced no words at all takes whisperx's
// segment-level path, where the arrow sequence is defused so it cannot be read
// as a cue separator.
func TestSubtitlesFallBackToSegmentsWithoutWords(t *testing.T) {
	res := &core.Result{
		Language: "ru",
		Segments: []core.Segment{{ID: 0, Start: 0.5, End: 2.0, Text: "стрелка --> сюда"}},
	}
	if got, want := srt(res), "1\n00:00:00,500 --> 00:00:02,000\nстрелка -> сюда\n\n"; got != want {
		t.Errorf("srt = %q, want %q", got, want)
	}
}

// Japanese and Chinese join without separators, as whisperx's
// LANGUAGES_WITHOUT_SPACES says.
func TestSubtitleTextJoinsWithoutSpacesForCJK(t *testing.T) {
	res := twoSegments(2.2)
	res.Language = "ja"
	res.Segments[0].Words = []core.Word{
		{Word: "こん", Start: 0.5, End: 1.1},
		{Word: "にちは", Start: 1.2, End: 2.04},
	}
	res.Segments[1].Words = nil

	if got := vtt(res); !strings.Contains(got, "こんにちは") {
		t.Errorf("vtt separated the words:\n%s", got)
	}
}
