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

// SRT renders SubRip.
func SRT(res *core.Result) string {
	var b strings.Builder
	for i, s := range res.Segments {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
			i+1, Timecode(s.Start, ','), Timecode(s.End, ','), s.Text)
	}
	return b.String()
}

// VTT renders WebVTT.
func VTT(res *core.Result) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, s := range res.Segments {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n",
			Timecode(s.Start, '.'), Timecode(s.End, '.'), s.Text)
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
