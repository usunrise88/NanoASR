package pipeline

import (
	"context"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
	"github.com/usunrise88/nanoasr/internal/vad"
)

// fakeDiarizer returns a fixed turn map and records what it was asked for.
type fakeDiarizer struct {
	turns    []diarize.Turn
	clusters int
	calls    int
}

func (d *fakeDiarizer) Process(_ context.Context, _ audio.PCM, numClusters int) ([]diarize.Turn, error) {
	d.calls++
	d.clusters = numClusters
	return d.turns, nil
}
func (d *fakeDiarizer) Close() error { return nil }

func twoSpeakerHarness(t *testing.T, d diarize.Diarizer) *harness {
	t.Helper()
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("алло слушаю здравствуйте это",
				[]string{"▁алло", "▁слушаю", "▁здравствуйте", "▁это"},
				[]float32{0.0, 0.8, 2.0, 2.9},
				[]float32{0.6, 0.7, 0.8, 0.7}),
		},
	}
	h := newHarness(t, silence(4), []vad.Segment{segment(0, 4)}, rec, Options{})
	h.pipeline.WithDiarizer(d)
	return h
}

func TestDiarizeSplitsSegmentsAndSummarisesSpeakers(t *testing.T) {
	d := &fakeDiarizer{turns: []diarize.Turn{
		{Start: 0, End: 1.6, Speaker: 0},
		{Start: 1.9, End: 4, Speaker: 1},
	}}
	h := twoSpeakerHarness(t, d)

	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Diarize: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Segments) != 2 {
		t.Fatalf("got %d segments, want one per speaker: %+v", len(got.Segments), got.Segments)
	}
	if got.Segments[0].Speaker == nil || *got.Segments[0].Speaker != "spk_0" {
		t.Errorf("first segment speaker = %v", got.Segments[0].Speaker)
	}
	if got.Segments[1].Speaker == nil || *got.Segments[1].Speaker != "spk_1" {
		t.Errorf("second segment speaker = %v", got.Segments[1].Speaker)
	}
	if len(got.Speakers) != 2 {
		t.Fatalf("speakers = %+v, want two", got.Speakers)
	}
	// Every word carries its own attribution and a confidence, which is what
	// SPEC §5.7 asks for and what the segment label is derived from.
	for _, s := range got.Segments {
		for _, w := range s.Words {
			if w.Speaker == nil {
				t.Errorf("word %q has no speaker", w.Word)
				continue
			}
			if w.SpeakerConfidence <= 0 {
				t.Errorf("word %q has speaker %q at confidence %v", w.Word, *w.Speaker, w.SpeakerConfidence)
			}
		}
	}
	// The whole-text has to be rebuilt from the segments the client receives,
	// or it would still read as the pre-split transcript.
	if got.Text != "алло слушаю здравствуйте это" {
		t.Errorf("text = %q", got.Text)
	}
	if hasWarning(got.Warnings, "diarization_unavailable") {
		t.Errorf("warnings %+v claim diarization is unavailable", got.Warnings)
	}
	// The stage is timed like every other.
	if _, ok := got.Stats.StagesMS["diarize"]; !ok {
		t.Error("stages_ms has no diarize entry")
	}
}

// num_speakers is the one clustering knob a caller may set, and it has to reach
// the diarizer rather than being stored and dropped.
func TestDiarizePassesTheRequestedSpeakerCount(t *testing.T) {
	d := &fakeDiarizer{turns: []diarize.Turn{{Start: 0, End: 4, Speaker: 0}}}
	h := twoSpeakerHarness(t, d)

	if _, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Diarize: true, NumSpeakers: 3}); err != nil {
		t.Fatal(err)
	}
	if d.clusters != 3 {
		t.Errorf("diarizer asked for %d clusters, want the requested 3", d.clusters)
	}
}

// Without diarize=true the second pass must not run at all: it costs another
// 0.3-0.5x RTF and the primary workload is single-speaker.
func TestDiarizeDoesNotRunUnlessAsked(t *testing.T) {
	d := &fakeDiarizer{turns: []diarize.Turn{{Start: 0, End: 4, Speaker: 0}}}
	h := twoSpeakerHarness(t, d)

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}
	if d.calls != 0 {
		t.Errorf("the diarizer ran %d times without being asked", d.calls)
	}
	if got.Speakers != nil {
		t.Errorf("speakers = %+v, want none", got.Speakers)
	}
	// SPEC §6: null rather than an empty string when diarization did not run.
	for _, s := range got.Segments {
		if s.Speaker != nil {
			t.Errorf("segment speaker = %q, want null", *s.Speaker)
		}
	}
}

// SPEC §5.7: split already separates the speakers, so diarization is skipped
// rather than run over one leg of a call.
func TestDiarizeSkippedUnderChannelSplit(t *testing.T) {
	d := &fakeDiarizer{turns: []diarize.Turn{{Start: 0, End: 4, Speaker: 0}}}

	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("алло", []string{"▁алло"}, []float32{0}, []float32{0.5}),
			timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.5}),
		},
	}
	tracks := []audio.PCM{
		{Samples: make([]float32, int(2*testRate)), SampleRate: testRate, SourceChannels: 2, Channel: 0},
		{Samples: make([]float32, int(2*testRate)), SampleRate: testRate, SourceChannels: 2, Channel: 1},
	}
	h := newHarnessWithDecoder(t, multiDecoder{tracks: tracks},
		[]vad.Segment{segment(0, 1)}, rec, Options{ChannelMode: core.ChannelSplit})
	h.pipeline.WithDiarizer(d)

	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Diarize: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.calls != 0 {
		t.Errorf("the diarizer ran %d times under channel_mode=split", d.calls)
	}
	w, ok := findWarning(got.Warnings, "diarization_skipped_split")
	if !ok {
		t.Fatalf("warnings %+v should say why diarization was skipped", got.Warnings)
	}
	// Deliberately not an _unavailable code: strict mode must not reject a
	// request that got the better answer.
	if _, degraded := firstCapabilityWarning([]core.Warning{w}); degraded {
		t.Error("the skip warning trips strict mode, which would fail a request that succeeded")
	}
}

// A server without the models says so, per request, rather than silently
// returning an unattributed transcript.
func TestDiarizeWarnsWhenNotConfigured(t *testing.T) {
	h := twoSpeakerHarness(t, nil)
	h.pipeline.WithDiarizer(nil)

	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, Diarize: true})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "diarization_unavailable") {
		t.Errorf("warnings %+v should say diarization is not configured", got.Warnings)
	}
}
