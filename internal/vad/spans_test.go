package vad

import (
	"reflect"
	"testing"
)

func TestMergeSpansCoalescesOverlaps(t *testing.T) {
	cases := []struct {
		name string
		sets [][]Span
		want []Span
	}{
		{
			name: "disjoint sets interleave in time order",
			sets: [][]Span{{{0, 10}, {30, 40}}, {{15, 20}}},
			want: []Span{{0, 10}, {15, 20}, {30, 40}},
		},
		{
			// Two legs talking at once is one stretch of speech, not two.
			name: "overlapping channels become one span",
			sets: [][]Span{{{0, 100}}, {{50, 150}}},
			want: []Span{{0, 150}},
		},
		{
			name: "one span swallowed by another disappears",
			sets: [][]Span{{{0, 100}}, {{20, 40}}},
			want: []Span{{0, 100}},
		},
		{
			// Touching spans have no gap between them, so they are one.
			name: "abutting spans join",
			sets: [][]Span{{{0, 50}}, {{50, 90}}},
			want: []Span{{0, 90}},
		},
		{
			name: "a single set is returned as it is",
			sets: [][]Span{{{5, 10}}},
			want: []Span{{5, 10}},
		},
		{
			name: "nothing in, nothing out",
			sets: [][]Span{{}, {}},
			want: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MergeSpans(c.sets...)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("MergeSpans = %v, want %v", got, c.want)
			}
		})
	}
}

// The ratio is what predicts processing time, so counting a two-channel overlap
// twice would not merely be untidy — it could report more speech than there is
// recording.
func TestSpeechRatioFromNeverExceedsOne(t *testing.T) {
	spans := MergeSpans([]Span{{0, 1000}}, []Span{{0, 1000}})
	if got := SpeechRatioFrom(spans, 1000); got != 1 {
		t.Errorf("SpeechRatio = %v, want exactly 1 for two channels fully speaking", got)
	}
}

// Silence is where every channel is quiet: a gap on one leg that the other leg
// fills is not silence anyone should see painted.
func TestSilencesFromNeedsEveryChannelQuiet(t *testing.T) {
	const rate = 16000
	left := []Span{{0, rate}}         // speaks second 0
	right := []Span{{rate, 2 * rate}} // speaks second 1
	spans := MergeSpans(left, right)

	if got := SilencesFrom(spans, 2*rate, rate, 300); len(got) != 0 {
		t.Errorf("silences = %v, want none: one leg or the other is always speaking", got)
	}

	// Extend the recording past both legs and the tail is genuinely silent.
	got := SilencesFrom(spans, 3*rate, rate, 300)
	if len(got) != 1 || got[0].Start != 2 || got[0].End != 3 {
		t.Errorf("silences = %v, want a single 2s-3s tail", got)
	}
}
