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
	"github.com/usunrise88/nanoasr/internal/diarize"
	"github.com/usunrise88/nanoasr/internal/job"
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
	// MaxSplitChannels and MaxDecodedBytes bound what one request may decode.
	// Split multiplies decoded memory by the channel count, and neither the
	// upload limit nor the queue's byte budget knows that: they count what
	// arrived, not what it expands into.
	MaxSplitChannels int
	MaxDecodedBytes  int64

	// HotwordsEnabled is the server's consent to spend a model instance on a
	// caller's bias list. HotwordsDefaultScore applies when the caller does not
	// name one — the OpenAI dialect maps prompt to hotwords without a score, so
	// there has to be a default.
	HotwordsEnabled      bool
	HotwordsDefaultScore float32

	BatchMaxSize    int
	BatchMaxSeconds int
	// NumThreads is the CPU budget one decode batch claims from the governor.
	// It must match the per-model thread count, or admission control counts
	// something other than what runs.
	NumThreads int

	Observer core.Observer
}

// WithDiarizer attaches the speaker pass. It is set after construction rather
// than taken by New because it is optional, server-wide and built from its own
// config block — threading it through New would put a nil in every caller that
// does not use it.
func (p *Pipeline) WithDiarizer(d diarize.Diarizer) *Pipeline {
	p.diarizer = d
	return p
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
	diarizer  diarize.Diarizer
	opt       Options

	// Supplied by Attach; nil on a server built without a queue.
	queue *job.Queue
	store job.Store
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
	return p.transcribe(ctx, newID("txn"), req)
}

// Run is job.Runner: the same pipeline, told which id to report under.
//
// A queued job was given its id when it was accepted, and progress events,
// stage reports and the final result all have to carry that id rather than a
// fresh one, or an SSE client watching job_abc never hears about its own work.
func (p *Pipeline) Run(ctx context.Context, id string, req core.Request) (*core.Result, error) {
	return p.transcribe(ctx, id, req)
}

func (p *Pipeline) transcribe(ctx context.Context, id string, req core.Request) (*core.Result, error) {
	started := time.Now()
	stages := stageTimer{observer: p.opt.Observer, ctx: ctx, jobID: id}
	stages.zero("post", "diarize")

	// warn collects degradations as the stages discover them. It is threaded
	// through rather than gathered at the end because only the stage that hit
	// the limitation knows which limitation it hit.
	var warn []core.Warning

	modelID, err := p.resolveModel(req)
	if err != nil {
		return nil, err
	}

	// One PCM per channel: several under channel_mode: split, one otherwise.
	tracks, err := runStage(&stages, "decode", func() ([]audio.PCM, error) { return p.decode(ctx, req) })
	if err != nil {
		return nil, err
	}

	lease, variantWarnings, err := p.acquire(ctx, modelID, req)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	warn = append(warn, variantWarnings...)

	// vad, asr and assemble run once per channel. They are timed inside, and
	// stageTimer accumulates, so stages_ms reports the total work rather than
	// whichever channel happened to finish last.
	decoded := make([]track, 0, len(tracks))
	for _, pcm := range tracks {
		t, err := p.channel(ctx, &stages, lease, req, pcm)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, t)
	}

	result := p.assemble(id, lease, req, tracks, decoded)

	// Diarization is a second pass over the whole recording, so it runs after
	// the transcript exists and rewrites it rather than being woven into it.
	spk, err := runStage(&stages, "diarize", func() (diarizeResult, error) {
		r, w, err := p.diarize(ctx, req, tracks, result.Segments)
		warn = append(warn, w...)
		return r, err
	})
	if err != nil {
		return nil, err
	}
	result.Segments = spk.segments
	result.Speakers = spk.speakers
	if len(spk.speakers) > 0 {
		result.Text = joinSegments(result.Segments)
	}

	warn = append(warn, pendingFeatures(req, lease.Recognizer.Capabilities())...)
	warn = append(warn, p.unsupportedOptions(req, lease)...)
	result.Warnings = append(result.Warnings, warn...)

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

func (p *Pipeline) decode(ctx context.Context, req core.Request) ([]audio.PCM, error) {
	rc, err := req.Audio.Open()
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "cannot read the uploaded file").WithCause(err)
	}
	defer rc.Close()

	pcm, _, err := p.decoder.Decode(ctx, rc, audio.Options{
		TargetSampleRate: p.opt.TargetSampleRate,
		ChannelMode:      p.channelMode(req),
		MaxDurationSec:   p.opt.MaxDuration.Seconds(),
		MaxSplitChannels: p.opt.MaxSplitChannels,
		MaxDecodedBytes:  p.opt.MaxDecodedBytes,
	})
	if err != nil {
		return nil, err
	}
	if len(pcm) == 0 || len(pcm[0].Samples) == 0 {
		return nil, core.Errorf(core.CodeInvalidRequest, "the file contains no audio")
	}
	return pcm, nil
}

// channelMode is the request's choice over the server's default. Everything
// that asks "are we splitting?" has to ask it this way: reading req.ChannelMode
// alone would miss a server configured to split, and reading the option alone
// would ignore the request.
func (p *Pipeline) channelMode(req core.Request) core.ChannelMode {
	if req.ChannelMode != "" {
		return req.ChannelMode
	}
	return p.opt.ChannelMode
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

// buildSegments turns one channel's recognitions into segments. It reports
// whether any of them carried real token timings, because a result whose spans
// are only VAD boundaries has to say so.
func (p *Pipeline) buildSegments(
	lease *pool.Lease,
	pcm audio.PCM,
	segments []vad.Segment,
	recognitions []asr.Recognition,
) ([]core.Segment, bool) {
	caps := lease.Recognizer.Capabilities()
	unit := lease.Recognizer.ModelingUnit()
	rate := pcm.SampleRate

	var out []core.Segment
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
		if pcm.Channel != 0 {
			for j := range ws {
				ws[j].Channel = pcm.Channel
			}
		}

		out = append(out, core.Segment{
			Start:         segStart,
			End:           segEnd,
			Text:          text,
			Channel:       pcm.Channel,
			Speaker:       nil,
			AvgConfidence: averageConfidence(ws),
			Words:         ws,
		})
	}
	return out, sawTokenTimings
}

// assemble merges every channel into one result.
//
// Duration, silence and the speech ratio are properties of the recording, not
// of a channel: they are computed over the union of what the channels found, so
// silence is where every leg was quiet. SegmentsTotal stays the count of VAD
// segments across the channels — len(Segments) is the transcript, and the two
// stop being the same number as soon as anything splits or drops one.
func (p *Pipeline) assemble(
	id string,
	lease *pool.Lease,
	req core.Request,
	pcm []audio.PCM,
	tracks []track,
) *core.Result {
	rate := pcm[0].SampleRate
	longest, vadTotal := 0, 0
	spanSets := make([][]vad.Span, 0, len(tracks))
	sawTokenTimings := false
	for _, t := range tracks {
		if t.samples > longest {
			longest = t.samples
		}
		vadTotal += t.vadSegments
		spanSets = append(spanSets, t.spans)
		sawTokenTimings = sawTokenTimings || t.tokenTimings
	}
	spans := vad.MergeSpans(spanSets...)

	duration := 0.0
	if rate > 0 {
		duration = float64(longest) / float64(rate)
	}

	result := &core.Result{
		ID:              id,
		Model:           lease.Manifest.Key(),
		Language:        resultLanguage(req, lease),
		Duration:        duration,
		TimestampSource: core.TimestampToken,
		Silence:         vad.SilencesFrom(spans, longest, rate, p.opt.MinSilenceMS),
		Segments:        merge(tracks),
		Stats: core.Stats{
			AudioDuration: duration,
			SegmentsTotal: vadTotal,
			SpeechRatio:   vad.SpeechRatioFrom(spans, longest),
		},
	}
	result.Text = joinSegments(result.Segments)

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
//
// Only the language check lives here now. Every other degradation is reported
// by the stage that discovers it — the acquire step knows why a variant was
// refused, the post-processing factory knows which stages it could build, the
// diarize step knows why it skipped — and a warning written where the knowledge
// is cannot drift out of step with what the code actually did.
func (p *Pipeline) unsupportedOptions(req core.Request, lease *pool.Lease) []core.Warning {
	var out []core.Warning

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

// The asynchronous surface. The pipeline owns core.Service, so it is what a
// dialect talks to; the queue and the store do the work behind it.
//
// Attach supplies them after construction, because the queue needs the pipeline
// as its Runner and the pipeline needs the queue to submit — a cycle no pair of
// constructors can express. Doing it in one place in wire.go is clearer than
// inventing an interface whose only purpose is to break the knot.
func (p *Pipeline) Attach(q *job.Queue, store job.Store) {
	p.queue = q
	p.store = store

	// Stage reports become job events here rather than in the caller, so the
	// queue's hub and the pipeline's observer are the same object by
	// construction. Wired by hand they can drift apart, and the failure is
	// silent: every stage is reported to a hub nobody is watching, and clients
	// see a job go from queued to succeeded with nothing in between.
	p.opt.Observer = job.NewProgress(q.Hub(), p.opt.Observer)
}

func (p *Pipeline) Submit(ctx context.Context, req core.Request) (*core.Job, error) {
	if p.queue == nil {
		return nil, core.Errorf(core.CodeNotImplemented, "this server has no job queue")
	}
	caller := core.CallerOf(ctx)
	req.APIKeyID = caller.KeyID

	// Interactive work jumps the batch backlog: someone waiting at a screen
	// should not queue behind a nightly bulk run. Which keys count as a person
	// is the operator's call, not the client's — see core.Caller.
	priority := job.PriorityBatch
	if caller.Interactive {
		priority = job.PriorityInteractive
	}
	return p.queue.Submit(ctx, req, priority)
}

func (p *Pipeline) Job(ctx context.Context, id string) (*core.Job, error) {
	rec, err := p.record(ctx, id)
	if err != nil {
		return nil, err
	}
	j := rec.Job
	if j.Status == core.JobQueued && p.queue != nil {
		j.Position = p.queue.Position(id)
	}
	return &j, nil
}

func (p *Pipeline) ListJobs(ctx context.Context, f core.JobFilter) (*core.JobPage, error) {
	if p.store == nil {
		return nil, core.Errorf(core.CodeNotImplemented, "this server has no job history")
	}
	// The ownership rule is applied here rather than in a handler, so a dialect
	// cannot leave it out by omission.
	caller := core.CallerOf(ctx)
	f.APIKeyID = caller.KeyID
	f.Admin = caller.Admin

	jobs, next, err := p.store.List(ctx, f)
	if err != nil {
		return nil, err
	}
	if p.queue != nil {
		for i := range jobs {
			if jobs[i].Status == core.JobQueued {
				jobs[i].Position = p.queue.Position(jobs[i].ID)
			}
		}
	}
	return &core.JobPage{Jobs: jobs, NextCursor: next}, nil
}

func (p *Pipeline) Cancel(ctx context.Context, id string) error {
	rec, err := p.record(ctx, id)
	if err != nil {
		return err
	}
	if job.Terminal(rec.Job.Status) {
		return nil // already over; cancelling it again changes nothing
	}
	return p.queue.Cancel(ctx, id)
}

func (p *Pipeline) Watch(ctx context.Context, id string, after int64) (<-chan core.JobEvent, error) {
	rec, err := p.record(ctx, id)
	if err != nil {
		return nil, err
	}

	events, cancel, live := p.queue.Hub().Subscribe(id, after)
	if !live {
		// Finished, or finished between the read above and here. One catch-up
		// event carrying the stored state, then the stream is over.
		//
		// Seq 0 means "no sequence": the transition this state came from is no
		// longer known, and inventing a number would give an unchanging job a
		// different event id on every reconnect.
		out := make(chan core.JobEvent, 1)
		out <- core.JobEvent{Job: rec.Job}
		close(out)
		return out, nil
	}

	out := make(chan core.JobEvent)
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				select {
				case out <- core.JobEvent{Seq: ev.Seq, Job: ev.Job}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// record reads a job and enforces the ownership rule.
//
// Someone else's job answers job_not_found rather than 403: a 403 confirms that
// a job with that id exists, which turns id guessing into a census of other
// people's activity.
func (p *Pipeline) record(ctx context.Context, id string) (job.Record, error) {
	if p.store == nil || p.queue == nil {
		return job.Record{}, core.Errorf(core.CodeNotImplemented, "this server has no job queue")
	}
	rec, err := p.store.Get(ctx, id)
	if err != nil {
		return job.Record{}, err
	}
	caller := core.CallerOf(ctx)
	if !caller.Admin && rec.APIKeyID != caller.KeyID {
		return job.Record{}, core.Errorf(core.CodeJobNotFound, "no such job: %s", id)
	}
	return rec, nil
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

// record adds to a stage rather than replacing it. Channel split runs vad, asr
// and assemble once per channel, and an assignment here would report only the
// last channel's time while the others vanished into the gap between
// processing_ms and the sum of the stages.
func (t *stageTimer) record(stage string, d time.Duration, err error) {
	if t.elapsed == nil {
		t.elapsed = map[string]int64{}
	}
	t.elapsed[stage] += d.Milliseconds()
	t.observer.StageFinished(t.ctx, t.jobID, stage, d.Milliseconds(), err)
}

// zero declares stages that must appear in stages_ms even when they did not
// run. SPEC §6 lists post and diarize in the result schema, and a client cannot
// tell an absent key from a stage that was skipped — 0 says "did not run",
// missing says "this build has never heard of it".
func (t *stageTimer) zero(stages ...string) {
	if t.elapsed == nil {
		t.elapsed = map[string]int64{}
	}
	for _, s := range stages {
		if _, ok := t.elapsed[s]; !ok {
			t.elapsed[s] = 0
		}
	}
}

func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
