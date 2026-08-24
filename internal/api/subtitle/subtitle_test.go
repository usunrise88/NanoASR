package subtitle

import (
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

func result() *core.Result {
	return &core.Result{
		Segments: []core.Segment{
			{ID: 0, Start: 0.5, End: 2.25, Text: "привет мир"},
			{ID: 1, Start: 3, End: 3661.007, Text: "вторая реплика"},
		},
	}
}

func TestTimecodeFormatting(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{0, "00:00:00,000"},
		{0.5, "00:00:00,500"},
		{61.25, "00:01:01,250"},
		{3661.007, "01:01:01,007"},
		{-1, "00:00:00,000"},
	}
	for _, c := range cases {
		if got := Timecode(c.seconds, ','); got != c.want {
			t.Errorf("Timecode(%.3f) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func TestSRTNumbersCuesFromOne(t *testing.T) {
	got := SRT(result())
	want := "1\n00:00:00,500 --> 00:00:02,250\nпривет мир\n\n" +
		"2\n00:00:03,000 --> 01:01:01,007\nвторая реплика\n\n"
	if got != want {
		t.Errorf("SRT =\n%q\nwant\n%q", got, want)
	}
}

func TestVTTStartsWithItsMagicLineAndUsesADot(t *testing.T) {
	got := VTT(result())
	if !strings.HasPrefix(got, "WEBVTT\n\n") {
		t.Fatalf("VTT does not start with WEBVTT: %q", got)
	}
	if !strings.Contains(got, "00:00:00.500 --> 00:00:02.250") {
		t.Errorf("VTT = %q, want dot-separated milliseconds", got)
	}
	if strings.Contains(got, ",250") {
		t.Errorf("VTT = %q, want no SRT-style comma", got)
	}
}

func TestEmptyResultRendersEmptyButValid(t *testing.T) {
	if got := SRT(&core.Result{}); got != "" {
		t.Errorf("SRT = %q, want empty", got)
	}
	if got := VTT(&core.Result{}); got != "WEBVTT\n\n" {
		t.Errorf("VTT = %q, want just the header", got)
	}
}
