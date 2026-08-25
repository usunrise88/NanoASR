// Package subtitle renders a result as SRT or WebVTT.
//
// It is its own package because two dialects offer the same formats, and a
// second copy of the timecode arithmetic is a second copy that can drift: a fix
// applied to one would not reach the other, and nothing would say so.
package subtitle

import (
	"fmt"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Content types, so a handler does not have to remember them.
const (
	SRTContentType = "application/x-subrip; charset=utf-8"
	VTTContentType = "text/vtt; charset=utf-8"
)

// Options controls what goes into a cue beyond the words.
type Options struct {
	// Speakers prefixes each cue with who said it. Off for an undiarized
	// result, where every cue would carry the same empty label.
	Speakers bool
}

// SRT renders SubRip, labelling speakers when the result has any.
func SRT(res *core.Result) string { return SRTWith(res, defaults(res)) }

// VTT renders WebVTT, labelling speakers when the result has any.
func VTT(res *core.Result) string { return VTTWith(res, defaults(res)) }

// defaults turns speaker labels on exactly when there are speakers to label, so
// an undiarized result renders byte for byte as it did before diarization
// existed.
func defaults(res *core.Result) Options {
	return Options{Speakers: len(res.Speakers) > 0}
}

// SRTWith renders SubRip.
//
// SubRip has no notion of a speaker, so the label goes inside the cue text as
// "spk_0: ". That is the convention players and editors already expect, and an
// unknown tag would simply be displayed verbatim.
func SRTWith(res *core.Result, o Options) string {
	var b strings.Builder
	for i, s := range res.Segments {
		text := s.Text
		if o.Speakers && s.Speaker != nil {
			text = *s.Speaker + ": " + text
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
			i+1, Timecode(s.Start, ','), Timecode(s.End, ','), text)
	}
	return b.String()
}

// VTTWith renders WebVTT.
//
// WebVTT has a real voice span, so it gets one: a player can style or filter
// <v spk_0> rather than having to parse a prefix back out of the words.
func VTTWith(res *core.Result, o Options) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, s := range res.Segments {
		text := s.Text
		if o.Speakers && s.Speaker != nil {
			text = "<v " + *s.Speaker + ">" + text + "</v>"
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n",
			Timecode(s.Start, '.'), Timecode(s.End, '.'), text)
	}
	return b.String()
}

// Timecode formats seconds as HH:MM:SS,mmm or HH:MM:SS.mmm — SRT and WebVTT
// differ only in that separator.
func Timecode(seconds float64, sep rune) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds)
	ms := int((seconds - float64(total)) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", total/3600, (total%3600)/60, total%60, sep, ms)
}
