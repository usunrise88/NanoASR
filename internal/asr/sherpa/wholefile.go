package sherpa

import (
	"context"
	"sync"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
)

const (
	// wholeFileFeedSeconds is how much of a segment is handed to a stream
	// before the decoder is run over what that made ready. Feeding a long
	// segment in one piece would make the feature extractor hold every frame of
	// it at once; feeding in slices keeps that bounded, and gives a cancelled
	// request somewhere to notice.
	wholeFileFeedSeconds = 10

	// wholeFileLeadSeconds of silence come before each segment. A cache-aware
	// encoder starts with an empty left context, and speech that begins on the
	// first frame is where it is weakest: measured on Nemotron's own sample
	// clips, the first word was dropped from the German and the Spanish one
	// ("Alles hat ein Ende" came back as "hat ein Ende") until half a second of
	// silence stood in front of it. The realtime path does not need this —
	// a microphone opens before anyone speaks — and a VAD segment is cut at the
	// onset of speech, so here it does. The timestamps are shifted back by the
	// same amount, so they stay relative to the segment.
	wholeFileLeadSeconds = 0.5

	// wholeFileTailSeconds of silence follow each segment, and this one is
	// measured, not rounded. A cache-aware encoder only decodes a whole chunk
	// (560 ms for the catalogued export) and wants some right context behind
	// it, so speech that stops at the cut — which is where a VAD segment ends —
	// has its last word left in a chunk that never fills. On Nemotron's export,
	// with the reference clip cut at the end of "мои": 0.5 s of tail lost the
	// word, 0.7 to 1.1 s kept it, and the closing full stop only arrived at
	// 1.3 s. The cost is about a second of extra audio per segment, which at
	// 0.4x real time is the price of not losing the end of every sentence.
	wholeFileTailSeconds = 1.3
)

// wholeFile lets a streaming model decode a finished recording, so that a model
// exported for the realtime API serves uploads like any other.
//
// A streaming model has no whole-file entry point: audio goes in, the decoder
// is run for as long as it reports being ready, and the transcript is read off
// the stream. That is exactly what a finished segment needs too, so this feeds
// the segment in, tells the stream the audio is over, drains it and reads the
// hypothesis. Each segment is one utterance: the stream is never reset, so the
// timestamps stay relative to the segment as asr.Recognition requires,
// whatever the model's own endpointing might have wanted to do.
//
// It is a thin adapter over asr.Streaming rather than over sherpa so that it
// can be tested with a scripted recogniser.
type wholeFile struct {
	stream asr.Streaming

	// mu serialises the decoder rounds. A Recognizer is shared by every worker
	// holding a lease on it, while the batch scratch inside the streaming
	// recogniser was written for the realtime engine's single goroutine. The
	// rounds are short and a batch is decoded together, so the lock costs
	// little; if profiling ever says otherwise, the way out is to verify that
	// sherpa-onnx's online decoders may run concurrently on different streams
	// and give Online.Decode a batch of its own per call.
	mu sync.Mutex
}

func newWholeFile(s asr.Streaming) *wholeFile { return &wholeFile{stream: s} }

// Decode transcribes each segment of the batch as one utterance.
func (w *wholeFile) Decode(ctx context.Context, batch [][]float32, sampleRate int, opt asr.DecodeOptions) ([]asr.Recognition, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sampleRate <= 0 {
		sampleRate = w.stream.SampleRate()
	}

	streams := make([]asr.Stream, 0, len(batch))
	defer func() {
		for _, s := range streams {
			s.Close()
		}
	}()
	longest := 0
	for _, samples := range batch {
		s, err := w.stream.NewStream()
		if err != nil {
			return nil, err
		}
		streams = append(streams, s)
		s.SetLanguage(opt.Language)
		if len(samples) > longest {
			longest = len(samples)
		}
	}

	lead := make([]float32, int(wholeFileLeadSeconds*float64(sampleRate)))
	for _, s := range streams {
		s.Accept(sampleRate, lead)
	}

	step := wholeFileFeedSeconds * sampleRate
	for off := 0; off < longest; off += step {
		for i, samples := range batch {
			if off >= len(samples) {
				continue
			}
			streams[i].Accept(sampleRate, samples[off:min(off+step, len(samples))])
		}
		if err := w.drain(ctx, streams); err != nil {
			return nil, err
		}
	}

	tail := make([]float32, int(wholeFileTailSeconds*float64(sampleRate)))
	for _, s := range streams {
		s.Accept(sampleRate, tail)
		s.Finish()
	}
	if err := w.drain(ctx, streams); err != nil {
		return nil, err
	}

	out := make([]asr.Recognition, len(streams))
	for i, s := range streams {
		h := s.Hypothesis()
		// No LogProbs and no Durations: a streaming result carries neither,
		// and the family does not claim Confidence.
		out[i] = asr.Recognition{Text: h.Text, Tokens: h.Tokens, Timestamps: unshift(h.Timestamps, wholeFileLeadSeconds)}
	}
	return out, nil
}

// drain runs the decoder until no stream has anything left to decode.
func (w *wholeFile) drain(ctx context.Context, streams []asr.Stream) error {
	ready := make([]asr.Stream, 0, len(streams))
	for {
		ready = ready[:0]
		for _, s := range streams {
			if s.Ready() {
				ready = append(ready, s)
			}
		}
		if len(ready) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		w.mu.Lock()
		w.stream.Decode(ready)
		w.mu.Unlock()
	}
}

func (w *wholeFile) Capabilities() core.Capabilities { return w.stream.Capabilities() }
func (w *wholeFile) ModelingUnit() string            { return w.stream.ModelingUnit() }
func (w *wholeFile) Close() error                    { return w.stream.Close() }

var _ asr.Recognizer = (*wholeFile)(nil)

// unshift moves timestamps back by the lead-in, so they are measured from the
// start of the segment and not from the start of the silence in front of it. A
// token placed inside the lead-in itself lands on zero.
func unshift(ts []float32, seconds float64) []float32 {
	if len(ts) == 0 {
		return ts
	}
	out := make([]float32, len(ts))
	for i, t := range ts {
		out[i] = max(t-float32(seconds), 0)
	}
	return out
}

// WholeFile decodes uploaded recordings with any streaming recogniser, each
// segment as one utterance — the adapter the streaming models of this package
// use, for the streaming models of other engines.
func WholeFile(s asr.Streaming) asr.Recognizer { return newWholeFile(s) }
