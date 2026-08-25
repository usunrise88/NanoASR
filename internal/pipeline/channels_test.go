package pipeline

import (
	"context"
	"io"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/vad"
)

// multiDecoder hands back a fixed set of tracks, standing in for a stereo file.
type multiDecoder struct{ tracks []audio.PCM }

func (multiDecoder) CanDecode(audio.Format) bool { return true }
func (d multiDecoder) Decode(context.Context, io.Reader, audio.Options) ([]audio.PCM, error) {
	return d.tracks, nil
}

// perChannelSegmenter answers with a different segment per channel, so the test
// can tell which channel a segment came from by when it starts.
type perChannelSegmenter struct{ byChannel map[int][]vad.Segment }

func (s perChannelSegmenter) Segment(_ context.Context, pcm audio.PCM) ([]vad.Segment, error) {
	return s.byChannel[pcm.Channel], nil
}
func (perChannelSegmenter) Close() error { return nil }

// Split is the reason Segment.Channel exists. The two legs of a call have to
// arrive as separate segments, labelled, and interleaved by time — a transcript
// that reads as one side followed by the other is not a conversation.
func TestTranscribeSplitLabelsAndInterleavesChannels(t *testing.T) {
	// Left speaks at 0s and 2s, right at 1s. Read in time order that is
	// left, right, left.
	segmenter := perChannelSegmenter{byChannel: map[int][]vad.Segment{
		0: {segment(0, 0.5), segment(2, 0.5)},
		1: {segment(1, 0.5)},
	}}

	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("алло", []string{"▁алло"}, []float32{0}, []float32{0.2}),
			timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.2}),
			timedRecognition("слушаю", []string{"▁слушаю"}, []float32{0}, []float32{0.2}),
		},
	}

	tracks := []audio.PCM{
		{Samples: make([]float32, int(3*testRate)), SampleRate: testRate, SourceChannels: 2, Channel: 0},
		{Samples: make([]float32, int(3*testRate)), SampleRate: testRate, SourceChannels: 2, Channel: 1},
	}

	models := pool.New(fakeRegistry{languages: []string{"ru"}},
		func(context.Context, registry.Manifest, string) (asr.Recognizer, error) { return rec, nil },
		pool.Options{MaxResidentModels: 2, MaxModelRSSMB: 4096})
	t.Cleanup(func() { _ = models.Close() })

	p := New(audio.NewRouter(multiDecoder{tracks: tracks}), segmenter, models, pool.NewGovernor(4),
		Options{DefaultModel: "test-model", ChannelMode: core.ChannelSplit})

	got, err := p.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Segments) != 3 {
		t.Fatalf("got %d segments, want 3 across both channels", len(got.Segments))
	}

	wantChannels := []int{0, 1, 0}
	for i, s := range got.Segments {
		if s.ID != i {
			t.Errorf("segment %d has ID %d: ids must index the merged list", i, s.ID)
		}
		if s.Channel != wantChannels[i] {
			t.Errorf("segment %d is on channel %d, want %d — the merge is not in time order",
				i, s.Channel, wantChannels[i])
		}
		for _, w := range s.Words {
			if w.Channel != s.Channel {
				t.Errorf("segment %d word %q reports channel %d, segment says %d",
					i, w.Word, w.Channel, s.Channel)
			}
		}
	}

	// Every channel's VAD work is counted, and none of it overwrote another's.
	if got.Stats.SegmentsTotal != 3 {
		t.Errorf("SegmentsTotal = %d, want 3 VAD segments across the channels", got.Stats.SegmentsTotal)
	}
	for _, stage := range []string{"vad", "asr", "assemble"} {
		if _, ok := got.Stats.StagesMS[stage]; !ok {
			t.Errorf("stages_ms has no %q entry", stage)
		}
	}
	// SPEC §6 lists these whether or not they ran.
	for _, stage := range []string{"post", "diarize"} {
		if _, ok := got.Stats.StagesMS[stage]; !ok {
			t.Errorf("stages_ms must always carry %q, even at zero", stage)
		}
	}
}

// Asking for split no longer produces a warning saying it was ignored, because
// it is not ignored any more.
func TestTranscribeSplitDoesNotWarn(t *testing.T) {
	segs := []vad.Segment{segment(0, 1)}
	rec := &fakeRecognizer{
		unit: "bpe", caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("да", []string{"▁да"}, []float32{0}, []float32{0.2}),
		},
	}

	h := newHarness(t, silence(2), segs, rec, Options{})
	got, err := h.pipeline.Transcribe(context.Background(),
		core.Request{Audio: &fakeSource{}, ChannelMode: core.ChannelSplit})
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(got.Warnings, "channel_split_unavailable") {
		t.Errorf("warnings %+v still claim split is unimplemented", got.Warnings)
	}
}
