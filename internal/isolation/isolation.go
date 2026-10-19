// Package isolation runs voice isolations: find the voices of a recording,
// wait for a person to pick one, keep that voice and remove the rest.
//
// An isolation is held in memory, and its audio in a directory of its own
// under storage.temp_dir, until isolation.ttl passes with nothing happening to
// it, until it is deleted, or until the server stops. That is the one place
// this server keeps audio between requests, and the reason is the person in
// the middle: the voices cannot be extracted before somebody has listened to
// them. Nothing about an isolation outlives the process — the directory is
// emptied at startup — so a restart is the same as every isolation expiring.
package isolation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/diarize"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/tse"
)

// SampleRate is the rate everything is kept and served at: the extractor's.
const SampleRate = tse.SampleRate

// Engine is the part of tse.Engine the service uses, so tests can stand in
// for the models.
type Engine interface {
	Samples(ctx context.Context, audio []float32, speakers []tse.Speaker) ([]tse.Voice, error)
	Extract(ctx context.Context, audio []float32, sample tse.Sample, others [][]float32, keep []tse.Span, progress func(float64)) ([]float32, error)
	// Embed is the speaker embedding the samples were chosen by, for a
	// sample a client placed by hand.
	Embed(samples []float32) ([]float32, error)
}

// Options configures the service.
type Options struct {
	// Dir holds every isolation's audio. It is emptied when the service
	// starts.
	Dir string
	// TTL is how long an isolation that is not working lives after the
	// last thing that happened to it.
	TTL time.Duration
	// Concurrency is how many isolations are analysed or extracted at once.
	Concurrency int
	// MaxIsolations bounds how many exist at once, which bounds the disk
	// they hold: at most two 16-bit copies of a recording each.
	MaxIsolations int
	// Threads is what one running isolation claims from Governor.
	Threads  int
	Governor *pool.Governor

	// Decode turns an upload into 16 kHz mono, enforcing the server's
	// limits on duration and size.
	Decode   func(ctx context.Context, r io.Reader) ([]float32, error)
	Diarizer diarize.Diarizer
	// Engine returns the extraction models, loading them on first use.
	Engine func(ctx context.Context) (Engine, error)
	// Submit queues a transcription job; nil means the server has no queue.
	Submit func(ctx context.Context, req core.Request) (*core.Job, error)
	// MP3 encodes the WAV read from in as MP3, and closes in when its
	// output is closed; nil means MP3 is not offered.
	MP3 func(ctx context.Context, in io.ReadCloser) (io.ReadCloser, error)

	Log *slog.Logger
	Now func() time.Time
}

// Service implements core.IsolationService.
type Service struct {
	opt Options

	mu       sync.Mutex
	sessions map[string]*session
	// pending is the work queue in order, for positions.
	pending []task
	wake    chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type taskKind int

const (
	analyse taskKind = iota
	extract
)

type task struct {
	id   string
	kind taskKind
}

type session struct {
	id    string
	owner string
	dir   string
	view  core.Isolation

	// numSpeakers is the request's hint to the diarizer.
	numSpeakers int
	speakers    []tse.Speaker
	samples     map[string]tse.Sample

	seq     int64
	subs    map[chan struct{}]struct{}
	cancel  context.CancelFunc
	deleted bool
}

// New prepares the service. Start runs it.
func New(opt Options) (*Service, error) {
	if opt.Diarizer == nil || opt.Engine == nil || opt.Decode == nil {
		return nil, fmt.Errorf("isolation: a diarizer, an engine and a decoder are required")
	}
	opt.Concurrency = max(1, opt.Concurrency)
	opt.MaxIsolations = max(1, opt.MaxIsolations)
	opt.Threads = max(1, opt.Threads)
	if opt.TTL <= 0 {
		opt.TTL = time.Hour
	}
	if opt.Governor == nil {
		opt.Governor = pool.NewGovernor(opt.Threads)
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	// Whatever is here belonged to a process that is gone, and so did the
	// isolations it was for.
	if err := os.RemoveAll(opt.Dir); err != nil {
		return nil, fmt.Errorf("isolation: cannot clear %s: %w", opt.Dir, err)
	}
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("isolation: cannot create %s: %w", opt.Dir, err)
	}
	return &Service{
		opt:      opt,
		sessions: map[string]*session{},
		wake:     make(chan struct{}, 1),
	}, nil
}

// Start runs the workers and the expiry sweep until ctx is done or Close.
func (s *Service) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	for range s.opt.Concurrency {
		s.wg.Add(1)
		go s.worker()
	}
	s.wg.Add(1)
	go s.sweep()
}

// Close stops the work, waits for it, and deletes every isolation.
func (s *Service) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	s.mu.Lock()
	for _, sess := range s.sessions {
		s.dropLocked(sess)
	}
	s.mu.Unlock()
	_ = os.RemoveAll(s.opt.Dir)
}

// Formats lists what Result produces here.
func (s *Service) Formats() []core.AudioFormat {
	if s.opt.MP3 != nil {
		return []core.AudioFormat{core.AudioWAV, core.AudioMP3}
	}
	return []core.AudioFormat{core.AudioWAV}
}

// --- requests ---------------------------------------------------------------

func (s *Service) Create(ctx context.Context, req core.IsolationRequest) (*core.Isolation, error) {
	if req.Audio == nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "no audio in the request").WithParam("file")
	}
	if req.NumSpeakers < 0 {
		return nil, core.Errorf(core.CodeInvalidRequest, "num_speakers must not be negative").WithParam("num_speakers")
	}
	if s.ctx == nil || s.ctx.Err() != nil {
		return nil, core.Errorf(core.CodeDraining, "the server is shutting down")
	}
	now := s.opt.Now()
	sess := &session{
		id:          newID(),
		owner:       core.CallerOf(ctx).KeyID,
		numSpeakers: req.NumSpeakers,
		subs:        map[chan struct{}]struct{}{},
		view: core.Isolation{
			Status:    core.IsolationQueued,
			Filename:  req.Audio.Filename(),
			Source:    req.Source,
			CreatedAt: now,
			ExpiresAt: now.Add(s.opt.TTL),
		},
	}
	sess.view.ID = sess.id
	sess.dir = filepath.Join(s.opt.Dir, sess.id)

	s.mu.Lock()
	if len(s.sessions) >= s.opt.MaxIsolations {
		s.mu.Unlock()
		return nil, core.Errorf(core.CodeQueueFull,
			"%d voice isolations are already open on this server; delete one or wait for it to expire",
			len(s.sessions))
	}
	s.sessions[sess.id] = sess
	s.mu.Unlock()

	if err := s.keepUpload(sess, req.Audio); err != nil {
		s.mu.Lock()
		s.dropLocked(sess)
		s.mu.Unlock()
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueueLocked(task{sess.id, analyse})
	v := s.viewLocked(sess)
	return &v, nil
}

// keepUpload copies the request's audio into the isolation's directory: the
// request is answered long before the audio is decoded.
func (s *Service) keepUpload(sess *session, src core.AudioSource) error {
	if err := os.MkdirAll(sess.dir, 0o700); err != nil {
		return core.Errorf(core.CodeInternal, "cannot prepare the isolation's directory").WithCause(err)
	}
	in, err := src.Open()
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(filepath.Join(sess.dir, uploadFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot keep the upload").WithCause(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return core.Errorf(core.CodeInternal, "cannot keep the upload").WithCause(err)
	}
	if err := out.Close(); err != nil {
		return core.Errorf(core.CodeInternal, "cannot keep the upload").WithCause(err)
	}
	return nil
}

const (
	uploadFile = "upload"
	sourceFile = "source.s16"
	resultFile = "result.s16"
)

func (s *Service) Isolation(ctx context.Context, id string) (*core.Isolation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	v := s.viewLocked(sess)
	return &v, nil
}

func (s *Service) Watch(ctx context.Context, id string, after int64) (<-chan core.IsolationEvent, error) {
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	note := make(chan struct{}, 1)
	sess.subs[note] = struct{}{}
	s.mu.Unlock()

	out := make(chan core.IsolationEvent)
	go func() {
		defer close(out)
		defer func() {
			s.mu.Lock()
			delete(sess.subs, note)
			s.mu.Unlock()
		}()
		last := after
		for {
			// The latest state, not every state: a watcher that falls
			// behind skips to the present instead of replaying percentages.
			s.mu.Lock()
			view, seq, gone := s.viewLocked(sess), sess.seq, sess.deleted
			s.mu.Unlock()
			if gone {
				return
			}
			if seq > last || last == 0 {
				select {
				case out <- core.IsolationEvent{Seq: seq, Isolation: view}:
					last = max(seq, 1)
				case <-ctx.Done():
					return
				}
			}
			// Over until a person acts: a client that wants the extraction
			// it starts next watches again.
			if !view.Status.Busy() {
				return
			}
			select {
			case <-note:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *Service) Sample(ctx context.Context, id, speaker string) (io.ReadCloser, error) {
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	sample, ok := sess.samples[speaker]
	known := slices.ContainsFunc(sess.view.Voices, func(v core.IsolationVoice) bool { return v.Speaker == speaker })
	s.mu.Unlock()
	switch {
	case ok:
		return wavReader(sample.Audio), nil
	case known:
		return nil, core.Errorf(core.CodeInvalidRequest,
			"voice %s has no sample: it speaks on its own for too little time", speaker).WithParam("speaker")
	default:
		return nil, core.Errorf(core.CodeInvalidRequest,
			"isolation %s has no voice %q", id, speaker).WithParam("speaker")
	}
}

func (s *Service) Extract(ctx context.Context, id string, sel core.IsolationSelection) (*core.Isolation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	switch sess.view.Status {
	case core.IsolationAwaitingSelection, core.IsolationDone:
	case core.IsolationFailed:
		if len(sess.view.Voices) == 0 {
			return nil, core.Errorf(core.CodeIsolationBusy,
				"isolation %s failed before its voices were found; start a new one", id)
		}
	default:
		return nil, core.Errorf(core.CodeIsolationBusy,
			"isolation %s is %s; a voice can be chosen once the voices are found and nothing is running",
			id, sess.view.Status)
	}
	var voice *core.IsolationVoice
	for i := range sess.view.Voices {
		if sess.view.Voices[i].Speaker == sel.Speaker {
			voice = &sess.view.Voices[i]
		}
	}
	if voice == nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "isolation %s has no voice %q", id, sel.Speaker).
			WithParam("speaker")
	}
	if sel.SampleStart != nil {
		st := *sel.SampleStart
		if math.IsNaN(st) || st < 0 || st+3 > sess.view.Duration+1e-6 {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"sample_start must leave 3 seconds before the end of the %.1f-second recording",
				sess.view.Duration).WithParam("sample_start")
		}
	} else if voice.Sample == nil {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"voice %s has no sample of its own (%s); give sample_start to take one from a place you choose",
			sel.Speaker, voice.Reason).WithParam("speaker")
	}

	// A new extraction replaces the last one; its result is gone the moment
	// it starts, so nobody downloads the old voice believing it is the new.
	_ = os.Remove(filepath.Join(sess.dir, resultFile))
	chosen := sel
	sess.view.Selected = &chosen
	sess.view.Result = nil
	sess.view.Error = nil
	sess.view.Status = core.IsolationQueued
	sess.view.Stage, sess.view.Percent = "", 0
	s.enqueueLocked(task{id, extract})
	v := s.viewLocked(sess)
	return &v, nil
}

func (s *Service) Result(ctx context.Context, id string, format core.AudioFormat) (io.ReadCloser, error) {
	if format == "" {
		format = core.AudioWAV
	}
	if !slices.Contains(s.Formats(), format) {
		if format == core.AudioMP3 {
			return nil, core.Errorf(core.CodeCapabilityUnavailable,
				"MP3 needs ffmpeg, which is not installed on this server; ask for wav").WithParam("format")
		}
		return nil, core.Errorf(core.CodeInvalidRequest, "format %q is not one of wav, mp3", format).
			WithParam("format")
	}
	f, err := s.openResult(ctx, id)
	if err != nil {
		return nil, err
	}
	if format == core.AudioMP3 {
		// The request's context: a client that hangs up stops the encoder.
		return s.opt.MP3(ctx, f)
	}
	return f, nil
}

// openResult opens the isolated voice as WAV. An open file survives the
// extraction that replaces it, so a download in flight finishes.
func (s *Service) openResult(ctx context.Context, id string) (*wavFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	if sess.view.Status != core.IsolationDone || sess.view.Result == nil {
		return nil, core.Errorf(core.CodeIsolationBusy,
			"isolation %s is %s and has no isolated voice yet", id, sess.view.Status)
	}
	f, err := openWAV(filepath.Join(sess.dir, resultFile))
	if err != nil {
		return nil, err
	}
	s.touchLocked(sess)
	return f, nil
}

func (s *Service) Transcribe(ctx context.Context, id string, req core.Request) (*core.Job, error) {
	if s.opt.Submit == nil {
		return nil, core.Errorf(core.CodeNotImplemented, "this server has no job queue")
	}
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if sess.view.Status != core.IsolationDone || sess.view.Result == nil {
		s.mu.Unlock()
		return nil, core.Errorf(core.CodeIsolationBusy,
			"isolation %s is %s and has no isolated voice to transcribe yet", id, sess.view.Status)
	}
	path := filepath.Join(sess.dir, resultFile)
	name := sess.view.Result.Name + ".wav"
	s.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		return nil, core.Errorf(core.CodeIsolationBusy, "the isolated voice is gone").WithCause(err)
	}
	// The queue copies the audio into its own spool, so the job does not
	// depend on this isolation outliving it.
	req.Audio = &resultSource{path: path, name: name, size: audio.WAVHeaderSize + info.Size()}
	job, err := s.opt.Submit(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if !sess.deleted && sess.view.Result != nil {
		sess.view.Result.JobID = job.ID
		s.changedLocked(sess)
	}
	s.mu.Unlock()
	return job, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return err
	}
	s.dropLocked(sess)
	return nil
}

// --- bookkeeping ------------------------------------------------------------

// getLocked finds an isolation the caller may see. Someone else's answers
// not found, as a job does, so ids cannot be probed.
func (s *Service) getLocked(ctx context.Context, id string) (*session, error) {
	sess, ok := s.sessions[id]
	caller := core.CallerOf(ctx)
	if !ok || sess.deleted || (!caller.Admin && sess.owner != caller.KeyID) {
		return nil, core.Errorf(core.CodeIsolationNotFound, "no such isolation: %s (it may have expired)", id)
	}
	return sess, nil
}

func (s *Service) viewLocked(sess *session) core.Isolation {
	v := sess.view
	v.Voices = slices.Clone(v.Voices)
	v.Warnings = slices.Clone(v.Warnings)
	if v.Result != nil {
		r := *v.Result
		v.Result = &r
	}
	if v.Status == core.IsolationDone {
		v.Formats = s.Formats()
	}
	if v.Status == core.IsolationQueued {
		for i, t := range s.pending {
			if t.id == sess.id {
				v.Position = i + 1
			}
		}
	}
	return v
}

// changedLocked records a change: watchers hear of it, and the expiry moves.
func (s *Service) changedLocked(sess *session) {
	sess.seq++
	s.touchLocked(sess)
	for note := range sess.subs {
		select {
		case note <- struct{}{}:
		default:
		}
	}
}

func (s *Service) touchLocked(sess *session) {
	sess.view.ExpiresAt = s.opt.Now().Add(s.opt.TTL)
}

func (s *Service) dropLocked(sess *session) {
	if sess.deleted {
		return
	}
	sess.deleted = true
	if sess.cancel != nil {
		sess.cancel()
	}
	delete(s.sessions, sess.id)
	s.pending = slices.DeleteFunc(s.pending, func(t task) bool { return t.id == sess.id })
	for note := range sess.subs {
		select {
		case note <- struct{}{}:
		default:
		}
	}
	// A worker still holding files open loses nothing it is reading; what it
	// writes next fails, and it gives up.
	_ = os.RemoveAll(sess.dir)
}

func (s *Service) enqueueLocked(t task) {
	s.pending = append(s.pending, t)
	for _, p := range s.pending {
		if sess, ok := s.sessions[p.id]; ok {
			s.changedLocked(sess)
		}
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) sweep() {
	defer s.wg.Done()
	every := min(time.Minute, max(time.Second, s.opt.TTL/4))
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.expire()
		}
	}
}

// expire deletes the isolations whose time is up. One that is working is
// never cut short; its time starts again when the work ends.
func (s *Service) expire() {
	now := s.opt.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if !sess.view.Status.Busy() && now.After(sess.view.ExpiresAt) {
			s.opt.Log.Info("voice isolation expired", "isolation", sess.id)
			s.dropLocked(sess)
		}
	}
}

// --- work -------------------------------------------------------------------

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		t, ok := s.next()
		if !ok {
			return
		}
		s.run(t)
	}
}

// next takes the oldest pending task, waiting for one.
func (s *Service) next() (task, bool) {
	for {
		s.mu.Lock()
		if len(s.pending) > 0 {
			t := s.pending[0]
			s.pending = s.pending[1:]
			for _, p := range s.pending {
				if sess, ok := s.sessions[p.id]; ok {
					s.changedLocked(sess) // positions moved
				}
			}
			if len(s.pending) > 0 {
				select {
				case s.wake <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
			return t, true
		}
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
			return task{}, false
		case <-s.wake:
		}
	}
}

func (s *Service) run(t task) {
	s.mu.Lock()
	sess, ok := s.sessions[t.id]
	if !ok || sess.deleted {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	sess.cancel = cancel
	s.mu.Unlock()
	defer cancel()

	if err := s.opt.Governor.Acquire(ctx, s.opt.Threads); err != nil {
		return
	}
	defer s.opt.Governor.Release(s.opt.Threads)

	var err error
	switch t.kind {
	case analyse:
		err = s.analyse(ctx, sess)
	case extract:
		err = s.extract(ctx, sess)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sess.cancel = nil
	if sess.deleted {
		return
	}
	if err != nil {
		if ctx.Err() != nil && s.ctx.Err() != nil {
			err = core.Errorf(core.CodeDraining, "the server stopped before the isolation finished")
		}
		ce := core.AsError(err)
		if ce.Code == core.CodeInternal {
			s.opt.Log.Error("voice isolation failed", "isolation", sess.id, "err", err)
		}
		sess.view.Status = core.IsolationFailed
		sess.view.Error = ce
		sess.view.Stage = ""
	}
	s.changedLocked(sess)
}

// stage reports progress; a percentage that has not moved is not news.
func (s *Service) stage(sess *session, status core.IsolationStatus, stage string, percent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.deleted {
		return
	}
	if sess.view.Status == status && sess.view.Stage == stage && sess.view.Percent == percent {
		return
	}
	sess.view.Status, sess.view.Stage, sess.view.Percent = status, stage, percent
	s.changedLocked(sess)
}

func (s *Service) analyse(ctx context.Context, sess *session) error {
	s.stage(sess, core.IsolationAnalysing, "decode", 0)
	upload := filepath.Join(sess.dir, uploadFile)
	f, err := os.Open(upload)
	if err != nil {
		return core.Errorf(core.CodeInternal, "the upload is gone").WithCause(err)
	}
	x, err := s.opt.Decode(ctx, f)
	_ = f.Close()
	if err != nil {
		return err
	}
	_ = os.Remove(upload)
	if len(x) == 0 {
		return core.Errorf(core.CodeInvalidRequest, "the file contains no audio")
	}
	if err := writePCM(filepath.Join(sess.dir, sourceFile), x); err != nil {
		return err
	}
	dur := float64(len(x)) / SampleRate
	s.mu.Lock()
	sess.view.Duration = dur
	s.mu.Unlock()

	s.stage(sess, core.IsolationAnalysing, "diarize", 10)
	turns, err := s.opt.Diarizer.Process(ctx, audio.PCM{Samples: x, SampleRate: SampleRate, SourceChannels: 1}, sess.numSpeakers)
	if err != nil {
		return err
	}
	speakers := speakersOf(turns)
	if len(speakers) == 0 {
		return core.Errorf(core.CodeInvalidRequest, "no voices were found in the recording")
	}

	s.stage(sess, core.IsolationAnalysing, "samples", 60)
	eng, err := s.opt.Engine(ctx)
	if err != nil {
		return err
	}
	found, err := eng.Samples(ctx, x, speakers)
	if err != nil {
		return err
	}

	voices := make([]core.IsolationVoice, 0, len(found))
	samples := map[string]tse.Sample{}
	for _, v := range found {
		cv := core.IsolationVoice{Speaker: v.Speaker.ID, Turns: len(v.Speaker.Spans)}
		for _, sp := range v.Speaker.Spans {
			cv.TotalSpeech += sp.End - sp.Start
		}
		cv.TotalSpeech = round2(cv.TotalSpeech)
		if v.Err != nil {
			var short tse.ErrTooLittleSpeech
			if errors.As(v.Err, &short) {
				cv.Reason = fmt.Sprintf("speaks on its own for %.1f s; a sample needs at least %.1f",
					short.Clean, tse.DefaultSelection().MinCleanSpeech)
			} else {
				cv.Reason = v.Err.Error()
			}
		} else {
			cv.Sample = &core.VoiceSample{Kind: string(v.Sample.Kind), Start: round2(v.Sample.Start),
				Similarity: round2(v.Sample.Similarity)}
			samples[v.Speaker.ID] = v.Sample
		}
		voices = append(voices, cv)
	}
	// The most talkative first: the voice somebody came for is rarely the
	// one that says two words.
	sort.SliceStable(voices, func(i, j int) bool { return voices[i].TotalSpeech > voices[j].TotalSpeech })

	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.deleted {
		return nil
	}
	sess.speakers, sess.samples = speakers, samples
	sess.view.Voices = voices
	sess.view.Status, sess.view.Stage, sess.view.Percent = core.IsolationAwaitingSelection, "", 100
	if len(samples) == 0 {
		sess.view.Warnings = append(sess.view.Warnings, core.Warning{Code: "isolation_no_samples",
			Message: "no voice speaks on its own long enough for a sample; give sample_start to choose one"})
	}
	return nil
}

func (s *Service) extract(ctx context.Context, sess *session) error {
	s.stage(sess, core.IsolationExtracting, "extract", 0)
	s.mu.Lock()
	sel := *sess.view.Selected
	var keep []tse.Span
	for _, sp := range sess.speakers {
		if sp.ID == sel.Speaker {
			keep = sp.Spans
		}
	}
	sample, haveSample := sess.samples[sel.Speaker]
	var others [][]float32
	for id, smp := range sess.samples {
		if id != sel.Speaker {
			others = append(others, smp.Embedding)
		}
	}
	s.mu.Unlock()

	x, err := readPCM(filepath.Join(sess.dir, sourceFile))
	if err != nil {
		return err
	}
	eng, err := s.opt.Engine(ctx)
	if err != nil {
		return err
	}
	if sel.SampleStart != nil || !haveSample {
		start := 0.0
		if sel.SampleStart != nil {
			start = *sel.SampleStart
		}
		sample, err = tse.SampleAt(x, start, embedderFunc(eng.Embed))
		if err != nil {
			return err
		}
	}
	y, err := eng.Extract(ctx, x, sample, others, keep, func(f float64) {
		s.stage(sess, core.IsolationExtracting, "extract", int(f*99))
	})
	if err != nil {
		return err
	}
	if err := writePCM(filepath.Join(sess.dir, resultFile), y); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.deleted {
		return nil
	}
	sess.view.Result = &core.IsolationResult{
		Speaker:    sel.Speaker,
		Name:       resultName(sess.view.Filename, sel.Speaker),
		Duration:   round2(float64(len(y)) / SampleRate),
		SampleRate: SampleRate,
	}
	sess.view.Status, sess.view.Stage, sess.view.Percent = core.IsolationDone, "", 100
	return nil
}

type embedderFunc func([]float32) ([]float32, error)

func (f embedderFunc) Embed(s []float32) ([]float32, error) { return f(s) }

// speakersOf groups turns by speaker, numbered in order of first appearance as
// the transcript's speakers are.
func speakersOf(turns []diarize.Turn) []tse.Speaker {
	sorted := slices.Clone(turns)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })
	index := map[int]int{}
	var out []tse.Speaker
	for _, t := range sorted {
		if t.End <= t.Start {
			continue
		}
		i, ok := index[t.Speaker]
		if !ok {
			i = len(out)
			index[t.Speaker] = i
			out = append(out, tse.Speaker{ID: fmt.Sprintf("spk_%d", i)})
		}
		out[i].Spans = append(out[i].Spans, tse.Span{Start: t.Start, End: t.End})
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "iso_" + hex.EncodeToString(b[:])
}

// resultName is what the isolated voice is called when it is downloaded or
// transcribed, without an extension: the upload's name with the speaker in it.
func resultName(upload, speaker string) string {
	base := filepath.Base(upload)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	if base == "" || base == "." || base == "/" {
		base = "audio"
	}
	return base + "-" + speaker
}

// --- files ------------------------------------------------------------------

// writePCM stores samples as raw 16-bit PCM, half the disk of float32 and the
// exact payload of the WAV they are served as.
func writePCM(path string, x []float32) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return core.Errorf(core.CodeInternal, "cannot write the isolation's audio").WithCause(err)
	}
	const block = 1 << 16
	for at := 0; at < len(x); at += block {
		if _, err := f.Write(audio.EncodePCM16(x[at:min(at+block, len(x))])); err != nil {
			_ = f.Close()
			return core.Errorf(core.CodeInternal, "cannot write the isolation's audio").WithCause(err)
		}
	}
	if err := f.Close(); err != nil {
		return core.Errorf(core.CodeInternal, "cannot write the isolation's audio").WithCause(err)
	}
	return nil
}

func readPCM(path string) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot read the isolation's audio").WithCause(err)
	}
	x := make([]float32, len(b)/2)
	for i := range x {
		x[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / 32768
	}
	return x, nil
}

// wavFile is a stored PCM file read as WAV: a header, then the file.
type wavFile struct {
	io.Reader
	f    *os.File
	size int64
}

func openWAV(path string) (*wavFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, core.Errorf(core.CodeIsolationBusy, "the isolated voice is gone").WithCause(err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, core.Errorf(core.CodeInternal, "cannot read the isolated voice").WithCause(err)
	}
	return &wavFile{
		Reader: io.MultiReader(bytes.NewReader(audio.WAVHeader(SampleRate, info.Size())), f),
		f:      f,
		size:   audio.WAVHeaderSize + info.Size(),
	}, nil
}

func (w *wavFile) Close() error { return w.f.Close() }

// Size is the length of the whole WAV, for Content-Length.
func (w *wavFile) Size() int64 { return w.size }

func wavReader(x []float32) io.ReadCloser {
	data := audio.EncodePCM16(x)
	return io.NopCloser(io.MultiReader(bytes.NewReader(audio.WAVHeader(SampleRate, int64(len(data)))), bytes.NewReader(data)))
}

// resultSource hands the isolated voice to the job queue as an upload.
type resultSource struct {
	path, name string
	size       int64
}

func (r *resultSource) Open() (io.ReadCloser, error) { return openWAV(r.path) }
func (r *resultSource) Filename() string             { return r.name }
func (r *resultSource) Size() int64                  { return r.size }
func (r *resultSource) Close() error                 { return nil }

var _ core.IsolationService = (*Service)(nil)
