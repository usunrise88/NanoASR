// Package pipeline turns an uploaded file into a transcript.
//
// It is the only implementation of core.Service, and it is deliberately linear:
// decode, segment, recognise, assemble. Each stage takes and returns explicit
// values rather than enriching a shared struct in place, so what any stage can
// affect is visible from its signature.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/vad"
	"github.com/usunrise88/nanoasr/internal/words"
)

// Options are the pipeline's slice of the server configuration.
type Options struct {
	DefaultModel     string
	MaxDuration      time.Duration
	TargetSampleRate int
	ChannelMode      core.ChannelMode
	MinSilenceMS     int

	BatchMaxSize    int
	BatchMaxSeconds int
	// NumThreads is the CPU budget one decode batch claims from the governor.
	// It must match the per-model thread count, or admission control counts
	// something other than what runs.
	NumThreads int

	Observer core.Observer
}

func (o Options) withDefaults() Options {
	if o.TargetSampleRate <= 0 {
		o.TargetSampleRate = 16000
	}
	if o.ChannelMode == "" {
		o.ChannelMode = core.ChannelDownmix
	}
	if o.MinSilenceMS <= 0 {
		o.MinSilenceMS = 300
	}
	if o.BatchMaxSize <= 0 {
		o.BatchMaxSize = 8
	}
	if o.BatchMaxSeconds <= 0 {
		o.BatchMaxSeconds = 60
	}
	if o.NumThreads <= 0 {
		o.NumThreads = 1
	}
	if o.Observer == nil {
		o.Observer = core.NopObserver{}
	}
	return o
}

// Pipeline implements core.Service.
type Pipeline struct {
	decoder   *audio.Router
	segmenter vad.Segmenter
	models    *pool.Pool
	governor  *pool.Governor
	opt       Options
}

func New(decoder *audio.Router, segmenter vad.Segmenter, models *pool.Pool, governor *pool.Governor, opt Options) *Pipeline {
	return &Pipeline{
		decoder:   decoder,
		segmenter: segmenter,
		models:    models,
		governor:  governor,
		opt:       opt.withDefaults(),
	}
}

func (p *Pipeline) Transcribe(ctx context.Context, req core.Request) (*core.Result, error) {
	started := time.Now()
	id := newID("txn")
	stages := stageTimer{observer: p.opt.Observer, ctx: ctx, jobID: id}

	modelID, err := p.resolveModel(req)
	if err != nil {
		return nil, err
	}

	pcm, err := runStage(&stages, "decode", func() (audio.PCM, error) { return p.decode(ctx, req) })
	if err != nil {
		return nil, err
	}

	segments, err := runStage(&stages, "vad", func() ([]vad.Segment, error) {
		return p.segmenter.Segment(ctx, pcm)
	})
	if err != nil {
		return nil, err
	}

	lease, err := p.models.Acquire(ctx, modelID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	recognitions, err := runStage(&stages, "asr", func() ([]asr.Recognition, error) {
		return p.recognise(ctx, lease.Recognizer, segments, pcm.SampleRate)
	})
	if err != nil {
		return nil, err
	}

	result := p.assemble(id, lease, req, pcm, segments, recognitions)
	result.Warnings = append(result.Warnings, p.unsupportedOptions(req, lease)...)

	if req.Strict {
		if w, degraded := firstCapabilityWarning(result.Warnings); degraded {
			return nil, core.Errorf(core.CodeCapabilityUnavailable, "strict mode: %s", w.Message)
		}
	}

	result.Stats.ProcessingMS = time.Since(started).Milliseconds()
	result.Stats.StagesMS = stages.elapsed
	if result.Duration > 0 {
		result.Stats.RTF = float64(result.Stats.ProcessingMS) / 1000 / result.Duration
	}
	return result, nil
}

func (p *Pipeline) resolveModel(req core.Request) (string, error) {
	if req.ModelID != "" {
		return req.ModelID, nil
	}
	if p.opt.DefaultModel != "" {
		return p.opt.DefaultModel, nil
	}
	return "", core.Errorf(core.CodeInvalidRequest,
		"no model requested and asr.default_model is not configured").WithParam("model")
}

func (p *Pipeline) decode(ctx context.Context, req core.Request) (audio.PCM, error) {
	rc, err := req.Audio.Open()
	if err != nil {
		return audio.PCM{}, core.Errorf(core.CodeInvalidRequest, "cannot read the uploaded file").WithCause(err)
	}
	defer rc.Close()

	mode := p.opt.ChannelMode
	if req.ChannelMode != "" {
		mode = req.ChannelMode
	}

	pcm, _, err := p.decoder.Decode(ctx, rc, audio.Options{
		TargetSampleRate: p.opt.TargetSampleRate,
		ChannelMode:      mode,
		MaxDurationSec:   p.opt.MaxDuration.Seconds(),
	})
	if err != nil {
		return audio.PCM{}, err
	}
	if len(pcm.Samples) == 0 {
		return audio.PCM{}, core.Errorf(core.CodeInvalidRequest, "the file contains no audio")
	}
	return pcm, nil
}

// recognise decodes the segments in batches.
//
// Batching is what keeps the CPU busy: VAD segments on telephony are often
// under two seconds, and decoding them one at a time leaves cores idle between
// utterances. The governor is claimed per batch rather than per file so a long
// recording cannot hold the whole CPU budget for minutes.
func (p *Pipeline) recognise(ctx context.Context, rec asr.Recognizer, segments []vad.Segment, sampleRate int) ([]asr.Recognition, error) {
	out := make([]asr.Recognition, 0, len(segments))
	maxSamples := p.opt.BatchMaxSeconds * sampleRate

	for start := 0; start < len(segments); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		end, samples := start, 0
		for end < len(segments) && end-start < p.opt.BatchMaxSize {
			next := samples + len(segments[end].Samples)
			if end > start && next > maxSamples {
				break
			}
			samples = next
			end++
		}

		batch := make([][]float32, 0, end-start)
		for _, s := range segments[start:end] {
			batch = append(batch, s.Samples)
		}

		if err := p.governor.Acquire(ctx, p.opt.NumThreads); err != nil {
			return nil, err
		}
		got, err := rec.Decode(ctx, batch, sampleRate)
		p.governor.Release(p.opt.NumThreads)
		if err != nil {
			return nil, err
		}

		out = append(out, got...)
		start = end
	}
	return out, nil
}

func (p *Pipeline) assemble(id string, lease *pool.Lease, req core.Request, pcm audio.PCM, segments []vad.Segment, recognitions []asr.Recognition) *core.Result {
	caps := lease.Recognizer.Capabilities()
	unit := lease.Recognizer.ModelingUnit()
	rate := pcm.SampleRate

	result := &core.Result{
		ID:              id,
		Model:           lease.Manifest.Key(),
		Language:        resultLanguage(req, lease),
		Duration:        pcm.Duration(),
		TimestampSource: core.TimestampToken,
		Silence:         vad.Silences(segments, len(pcm.Samples), rate, p.opt.MinSilenceMS),
		Stats: core.Stats{
			AudioDuration: pcm.Duration(),
			SegmentsTotal: len(segments),
			SpeechRatio:   vad.SpeechRatio(segments, len(pcm.Samples)),
		},
	}

	var texts []string
	sawTokenTimings := false

	for i, s := range segments {
		if i >= len(recognitions) {
			break
		}
		rec := recognitions[i]
		text := strings.TrimSpace(rec.Text)
		if text == "" {
			continue
		}

		segStart := float64(s.StartSample) / float64(rate)
		segEnd := float64(s.EndSample()) / float64(rate)

		ws := words.Assemble(rec, words.Options{
			ModelingUnit:   unit,
			SegmentStart:   segStart,
			SegmentEnd:     segEnd,
			WithConfidence: caps.Confidence,
		})
		if ws == nil {
			// The model produced no token timings. Fall back to the VAD
			// boundaries and say so, rather than inventing word times.
			ws = words.FromSegment(text, segStart, segEnd)
		} else {
			sawTokenTimings = true
		}

		texts = append(texts, text)
		result.Segments = append(result.Segments, core.Segment{
			ID:            len(result.Segments),
			Start:         segStart,
			End:           segEnd,
			Text:          text,
			Channel:       0,
			Speaker:       nil,
			AvgConfidence: averageConfidence(ws),
			Words:         ws,
		})
	}

	result.Text = strings.Join(texts, " ")

	if len(result.Segments) == 0 {
		result.Warnings = append(result.Warnings, core.Warning{
			Code:    "no_speech_detected",
			Message: "the recording contains no speech the model could transcribe",
		})
	}
	if !sawTokenTimings && len(result.Segments) > 0 {
		result.TimestampSource = core.TimestampSegment
		result.Warnings = append(result.Warnings, core.Warning{
			Code: "word_timestamps_unavailable",
			Message: fmt.Sprintf("model %s returned no token timings; word spans are VAD segment boundaries",
				lease.Manifest.Key()),
		})
	}
	return result
}

// unsupportedOptions reports request options this build accepts but does not
// act on. Answering with a transcript that quietly ignored diarize=true is
// worse than saying so.
func (p *Pipeline) unsupportedOptions(req core.Request, lease *pool.Lease) []core.Warning {
	var out []core.Warning

	if req.Diarize {
		out = append(out, core.Warning{
			Code:    "diarization_unavailable",
			Message: "diarization is not implemented in this build; every word is unattributed",
		})
	}
	if req.Punctuate {
		out = append(out, core.Warning{
			Code:    "punctuation_unavailable",
			Message: "punctuation restoration is not implemented in this build",
		})
	}
	if req.ITN {
		out = append(out, core.Warning{
			Code:    "itn_unavailable",
			Message: "inverse text normalisation is not implemented in this build",
		})
	}
	if len(req.Hotwords) > 0 {
		out = append(out, core.Warning{
			Code:    "hotwords_unavailable",
			Message: "hotword biasing is not implemented in this build; the words were ignored",
		})
	}
	if req.ChannelMode == core.ChannelSplit {
		out = append(out, core.Warning{
			Code:    "channel_split_unavailable",
			Message: "per-channel recognition is not implemented in this build; channels were downmixed",
		})
	}
	if req.Language != "" && !lease.Manifest.Supports(req.Language) {
		out = append(out, core.Warning{
			Code: "language_mismatch",
			Message: fmt.Sprintf("model %s does not list %q among its languages (%v)",
				lease.Manifest.Key(), req.Language, lease.Manifest.Languages),
		})
	}
	return out
}

// firstCapabilityWarning finds a warning that means the server could not do
// what was asked. An empty recording is a fact about the audio, not a
// degradation, so strict mode does not reject it.
func firstCapabilityWarning(ws []core.Warning) (core.Warning, bool) {
	for _, w := range ws {
		if strings.HasSuffix(w.Code, "_unavailable") || w.Code == "language_mismatch" {
			return w, true
		}
	}
	return core.Warning{}, false
}

func resultLanguage(req core.Request, lease *pool.Lease) string {
	if req.Language != "" {
		return req.Language
	}
	if len(lease.Manifest.Languages) > 0 {
		return lease.Manifest.Languages[0]
	}
	return ""
}

func averageConfidence(ws []core.Word) float64 {
	sum, n := 0.0, 0
	for _, w := range ws {
		if w.Confidence > 0 {
			sum += w.Confidence
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// The asynchronous surface belongs to the queue, which is a later milestone.
// Returning a clear not_implemented is better than a half-working queue that
// loses jobs on restart.

func (p *Pipeline) Submit(context.Context, core.Request) (*core.Job, error) {
	return nil, core.ErrNotImplemented
}

func (p *Pipeline) Job(context.Context, string) (*core.Job, error) {
	return nil, core.ErrNotImplemented
}

func (p *Pipeline) ListJobs(context.Context, core.JobFilter) ([]core.Job, error) {
	return nil, core.ErrNotImplemented
}

func (p *Pipeline) Cancel(context.Context, string) error {
	return core.ErrNotImplemented
}

func (p *Pipeline) Watch(context.Context, string) (<-chan core.Job, error) {
	return nil, core.ErrNotImplemented
}

var _ core.Service = (*Pipeline)(nil)

// stageTimer records per-stage durations and reports them to the observer,
// which is a no-op until metrics land.
type stageTimer struct {
	ctx      context.Context
	jobID    string
	observer core.Observer
	elapsed  map[string]int64
}

func (t *stageTimer) record(stage string, d time.Duration, err error) {
	if t.elapsed == nil {
		t.elapsed = map[string]int64{}
	}
	t.elapsed[stage] = d.Milliseconds()
	t.observer.StageFinished(t.ctx, t.jobID, stage, d.Milliseconds(), err)
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
