// Package isolation runs voice isolations: find the voices of a recording,
// wait for a person to pick one, keep that voice and remove the rest — or keep
// every voice on a track of its own, and transcribe the tracks into one
// transcript that knows who said what.
//
// An isolation is held in memory, and its audio in a directory of its own
// under Options.Dir, until isolation.ttl passes with nothing happening to it,
// until it is deleted, or until the server stops. That is the reason this
// server keeps audio between requests at all: the voices cannot be extracted
// before somebody has listened to them. A person may also ask for an
// isolation to be kept: it then has a state file beside its audio, never
// expires, and is loaded again when the server starts. Everything else in the
// directory belonged to a process that is gone, and is deleted at startup.
package isolation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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

// Jobs is the part of the transcription service the isolations submit their
// tracks to. core.Service is one.
type Jobs interface {
	Submit(ctx context.Context, req core.Request) (*core.Job, error)
	Job(ctx context.Context, id string) (*core.Job, error)
	Watch(ctx context.Context, id string, after int64) (<-chan core.JobEvent, error)
	Cancel(ctx context.Context, id string) error
}

// Options configures the service.
type Options struct {
	// Dir holds every isolation's audio. What is not a kept isolation is
	// deleted when the service starts.
	Dir string
	// TTL is how long an isolation that is not working lives after the
	// last thing that happened to it.
	TTL time.Duration
	// Concurrency is how many isolations are analysed or extracted at once.
	Concurrency int
	// MaxIsolations bounds how many isolations that are not kept exist at
	// once, which bounds the disk they hold: a 16-bit copy of the recording
	// and one per voice isolated.
	MaxIsolations int
	// AllowKeep lets a person keep an isolation past its TTL; MaxKept bounds
	// how many are.
	AllowKeep bool
	MaxKept   int
	// Threads is what one running isolation claims from Governor.
	Threads  int
	Governor *pool.Governor

	// Decode turns an upload into 16 kHz mono, enforcing the server's
	// limits on duration and size.
	Decode   func(ctx context.Context, r io.Reader) ([]float32, error)
	Diarizer diarize.Diarizer
	// Engine returns the extraction models, loading them on first use.
	Engine func(ctx context.Context) (Engine, error)
	// Jobs transcribes the tracks; nil means the server has no queue.
	Jobs Jobs
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
	// transcribe isolates the voices it names, then hands every track to
	// the job queue.
	transcribe
)

type task struct {
	id     string
	kind   taskKind
	voices []voiceTask
	req    core.Request
}

// voiceTask is one voice to isolate, from the server's sample or from one a
// person placed.
type voiceTask struct {
	speaker     string
	sampleStart *float64
}

type session struct {
	id string
	// caller is who created it: it owns the isolation, and the jobs the
	// isolation submits for it are its jobs.
	caller core.Caller
	dir    string
	view   core.Isolation

	// numSpeakers is the request's hint to the diarizer.
	numSpeakers int
	speakers    []tse.Speaker
	samples     map[string]tse.Sample

	seq     int64
	subs    map[chan struct{}]struct{}
	cancel  context.CancelFunc
	deleted bool
}

// New prepares the service, loading the isolations that were kept. Start
// runs it.
func New(opt Options) (*Service, error) {
	if opt.Diarizer == nil || opt.Engine == nil || opt.Decode == nil {
		return nil, fmt.Errorf("isolation: a diarizer, an engine and a decoder are required")
	}
	opt.Concurrency = max(1, opt.Concurrency)
	opt.MaxIsolations = max(1, opt.MaxIsolations)
	opt.MaxKept = max(1, opt.MaxKept)
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
	if err := os.MkdirAll(opt.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("isolation: cannot create %s: %w", opt.Dir, err)
	}
	s := &Service{
		opt:      opt,
		sessions: map[string]*session{},
		wake:     make(chan struct{}, 1),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
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

// Close stops the work, waits for it, and deletes every isolation that is not
// kept. A kept one stays on disk as it was last saved.
func (s *Service) Close() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if !sess.view.Keep {
			s.dropLocked(sess)
		}
	}
}

// Formats lists what Result produces here.
func (s *Service) Formats() []core.AudioFormat {
	if s.opt.MP3 != nil {
		return []core.AudioFormat{core.AudioWAV, core.AudioMP3}
	}
	return []core.AudioFormat{core.AudioWAV}
}

// CanKeep reports whether a person may keep an isolation here.
func (s *Service) CanKeep() bool { return s.opt.AllowKeep }

// --- requests ---------------------------------------------------------------

func (s *Service) Create(ctx context.Context, req core.IsolationRequest) (*core.Isolation, error) {
	if req.Audio == nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "no audio in the request").WithParam("file")
	}
	if req.NumSpeakers < 0 {
		return nil, core.Errorf(core.CodeInvalidRequest, "num_speakers must not be negative").WithParam("num_speakers")
	}
	if req.Keep && !s.opt.AllowKeep {
		return nil, errNoKeeping()
	}
	if s.ctx == nil || s.ctx.Err() != nil {
		return nil, core.Errorf(core.CodeDraining, "the server is shutting down")
	}
	now := s.opt.Now()
	sess := &session{
		id:          newID(),
		caller:      core.CallerOf(ctx),
		numSpeakers: req.NumSpeakers,
		subs:        map[chan struct{}]struct{}{},
		view: core.Isolation{
			Status:    core.IsolationQueued,
			Filename:  req.Audio.Filename(),
			Source:    req.Source,
			Keep:      req.Keep,
			CreatedAt: now,
		},
	}
	sess.view.ID = sess.id
	sess.dir = filepath.Join(s.opt.Dir, sess.id)
	s.touchLocked(sess)

	s.mu.Lock()
	if err := s.roomLocked(req.Keep); err != nil {
		s.mu.Unlock()
		return nil, err
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
	s.enqueueLocked(task{id: sess.id, kind: analyse})
	s.saveLocked(sess)
	v := s.viewLocked(sess)
	return &v, nil
}

// roomLocked refuses an isolation over the limit for its kind: the ones that
// expire are bounded apart from the ones kept, so a shelf of kept recordings
// does not stop anybody isolating a voice today.
func (s *Service) roomLocked(keep bool) error {
	n := 0
	for _, sess := range s.sessions {
		if sess.view.Keep == keep {
			n++
		}
	}
	switch {
	case keep && n >= s.opt.MaxKept:
		return core.Errorf(core.CodeQueueFull,
			"%d voice isolations are kept on this server already; delete one first", n)
	case !keep && n >= s.opt.MaxIsolations:
		return core.Errorf(core.CodeQueueFull,
			"%d voice isolations are already open on this server; delete one or wait for it to expire", n)
	}
	return nil
}

func errNoKeeping() error {
	return core.Errorf(core.CodeCapabilityUnavailable,
		"this server does not keep isolations; set isolation.allow_keep").WithParam("keep")
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
	uploadFile     = "upload"
	sourceFile     = "source.s16"
	stateFile      = "state.json"
	transcriptFile = "transcript.json"
)

// trackFile and sampleFile are named by speaker id, which the service makes
// itself: a name a client sent never reaches the file system.
func trackFile(speaker string) string  { return "track-" + speaker + ".s16" }
func sampleFile(speaker string) string { return "sample-" + speaker + ".s16" }

func (s *Service) List(ctx context.Context) ([]core.Isolation, error) {
	caller := core.CallerOf(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []core.Isolation{}
	for _, sess := range s.sessions {
		if sess.deleted || (!caller.Admin && sess.caller.KeyID != caller.KeyID) {
			continue
		}
		out = append(out, s.viewLocked(sess))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

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
	known := sess.voiceLocked(speaker) != nil
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

// maxName bounds a voice's name: it labels transcript lines and names files.
const maxName = 64

func (s *Service) Update(ctx context.Context, id string, upd core.IsolationUpdate) (*core.Isolation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	// Everything is checked before anything changes: half an update is
	// one nobody asked for.
	names := map[string]string{}
	for speaker, name := range upd.Names {
		if sess.voiceLocked(speaker) == nil {
			return nil, core.Errorf(core.CodeInvalidRequest, "isolation %s has no voice %q", id, speaker).
				WithParam("names")
		}
		name = cleanName(name)
		if utf8.RuneCountInString(name) > maxName {
			return nil, core.Errorf(core.CodeInvalidRequest, "a voice's name is at most %d characters", maxName).
				WithParam("names")
		}
		names[speaker] = name
	}
	if upd.Keep != nil && *upd.Keep != sess.view.Keep {
		if *upd.Keep && !s.opt.AllowKeep {
			return nil, errNoKeeping()
		}
		if err := s.roomLocked(*upd.Keep); err != nil {
			return nil, err
		}
	}

	for speaker, name := range names {
		sess.voiceLocked(speaker).Name = name
	}
	if len(names) > 0 {
		// The tracks are called after their voices.
		for i := range sess.view.Tracks {
			sess.view.Tracks[i].Name = resultName(sess.view.Filename, sess.displayLocked(sess.view.Tracks[i].Speaker))
		}
	}
	if upd.Keep != nil && *upd.Keep != sess.view.Keep {
		sess.view.Keep = *upd.Keep
		if !sess.view.Keep {
			// Released: it lives as long as any other from now on.
			_ = os.Remove(filepath.Join(sess.dir, stateFile))
		}
	}
	s.changedLocked(sess)
	s.saveLocked(sess)
	v := s.viewLocked(sess)
	return &v, nil
}

// cleanName drops what has no place in a label or a file name: control
// characters, and space at either end.
func cleanName(name string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
}

// idleLocked checks that nothing is running, so a voice can be chosen.
func (s *Service) idleLocked(sess *session) error {
	switch sess.view.Status {
	case core.IsolationAwaitingSelection, core.IsolationDone:
		return nil
	case core.IsolationFailed:
		if len(sess.view.Voices) == 0 {
			return core.Errorf(core.CodeIsolationBusy,
				"isolation %s failed before its voices were found; start a new one", sess.id)
		}
		return nil
	}
	return core.Errorf(core.CodeIsolationBusy,
		"isolation %s is %s; a voice can be chosen once the voices are found and nothing is running",
		sess.id, sess.view.Status)
}

func (s *Service) Extract(ctx context.Context, id string, sel core.IsolationSelection) (*core.Isolation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.idleLocked(sess); err != nil {
		return nil, err
	}
	voice := sess.voiceLocked(sel.Speaker)
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

	chosen := sel
	sess.view.Selected = &chosen
	s.startLocked(sess, task{id: id, kind: extract, voices: []voiceTask{{sel.Speaker, sel.SampleStart}}})
	v := s.viewLocked(sess)
	return &v, nil
}

func (s *Service) ExtractAll(ctx context.Context, id string) (*core.Isolation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.idleLocked(sess); err != nil {
		return nil, err
	}
	voices, err := s.missingLocked(sess)
	if err != nil {
		return nil, err
	}
	if len(voices) == 0 {
		// Every voice has its track: there is nothing to do, and asking
		// twice is not a mistake.
		v := s.viewLocked(sess)
		return &v, nil
	}
	s.startLocked(sess, task{id: id, kind: extract, voices: voices})
	v := s.viewLocked(sess)
	return &v, nil
}

// missingLocked lists the voices that can be isolated and have no track yet.
// A voice without a sample of its own is left out with a warning: only a
// person can say where to take one from.
func (s *Service) missingLocked(sess *session) ([]voiceTask, error) {
	var out []voiceTask
	var skipped []string
	for _, v := range sess.view.Voices {
		if sess.trackLocked(v.Speaker) != nil {
			continue
		}
		if v.Sample == nil {
			skipped = append(skipped, sess.displayLocked(v.Speaker))
			continue
		}
		out = append(out, voiceTask{speaker: v.Speaker})
	}
	if len(out) == 0 && len(sess.view.Tracks) == 0 {
		return nil, core.Errorf(core.CodeInvalidRequest,
			"no voice has a sample of its own; isolate one with sample_start first")
	}
	if len(skipped) > 0 {
		addWarning(&sess.view.Warnings, core.Warning{Code: "isolation_voices_skipped",
			Message: fmt.Sprintf("not isolated, for want of a sample: %s; isolate them one by one with sample_start",
				strings.Join(skipped, ", "))})
	}
	return out, nil
}

// startLocked queues work on the isolation. The tracks it will make are gone
// the moment it starts, so nobody downloads an old voice believing it is the
// new one.
func (s *Service) startLocked(sess *session, t task) {
	for _, v := range t.voices {
		s.dropTrackLocked(sess, v.speaker)
	}
	sess.view.Error = nil
	sess.view.Status = core.IsolationQueued
	sess.view.Stage, sess.view.Percent, sess.view.Voice = "", 0, ""
	s.enqueueLocked(t)
	s.saveLocked(sess)
}

func (s *Service) dropTrackLocked(sess *session, speaker string) {
	i := slices.IndexFunc(sess.view.Tracks, func(r core.IsolationResult) bool { return r.Speaker == speaker })
	if i < 0 {
		return
	}
	sess.view.Tracks = slices.Delete(sess.view.Tracks, i, i+1)
	_ = os.Remove(filepath.Join(sess.dir, trackFile(speaker)))
	if t := sess.view.Transcript; t != nil && t.Status == core.JobSucceeded {
		t.Stale = true
	}
}

func (s *Service) Audio(ctx context.Context, id string, format core.AudioFormat) (io.ReadCloser, error) {
	if err := s.checkFormat(format); err != nil {
		return nil, err
	}
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	decoded := sess.view.Duration > 0
	path := filepath.Join(sess.dir, sourceFile)
	s.mu.Unlock()
	if !decoded {
		return nil, core.Errorf(core.CodeIsolationBusy, "isolation %s has not decoded its recording yet", id)
	}
	f, err := openWAV(path)
	if err != nil {
		return nil, err
	}
	return s.encode(ctx, f, format)
}

func (s *Service) checkFormat(format core.AudioFormat) error {
	if slices.Contains(s.Formats(), format) {
		return nil
	}
	if format == core.AudioMP3 {
		return core.Errorf(core.CodeCapabilityUnavailable,
			"MP3 needs ffmpeg, which is not installed on this server; ask for wav").WithParam("format")
	}
	return core.Errorf(core.CodeInvalidRequest, "format %q is not one of wav, mp3", format).WithParam("format")
}

func (s *Service) encode(ctx context.Context, f *wavFile, format core.AudioFormat) (io.ReadCloser, error) {
	if format == core.AudioMP3 {
		// The request's context: a client that hangs up stops the encoder.
		return s.opt.MP3(ctx, f)
	}
	return f, nil
}

func (s *Service) Result(ctx context.Context, id, speaker string, format core.AudioFormat) (io.ReadCloser, error) {
	if format == "" {
		format = core.AudioWAV
	}
	if err := s.checkFormat(format); err != nil {
		return nil, err
	}
	f, _, err := s.openTrack(ctx, id, speaker)
	if err != nil {
		return nil, err
	}
	return s.encode(ctx, f, format)
}

// openTrack opens a voice's track as WAV. An open file survives the
// extraction that replaces it, so a download in flight finishes.
func (s *Service) openTrack(ctx context.Context, id, speaker string) (*wavFile, core.IsolationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		return nil, core.IsolationResult{}, err
	}
	track, err := s.pickTrackLocked(sess, speaker)
	if err != nil {
		return nil, core.IsolationResult{}, err
	}
	f, err := openWAV(filepath.Join(sess.dir, trackFile(track.Speaker)))
	if err != nil {
		return nil, core.IsolationResult{}, err
	}
	s.touchLocked(sess)
	return f, *track, nil
}

// pickTrackLocked finds the track a request means: the speaker's, or with
// none named the voice chosen last, or the only one there is.
func (s *Service) pickTrackLocked(sess *session, speaker string) (*core.IsolationResult, error) {
	if speaker == "" {
		if sess.view.Selected != nil && sess.trackLocked(sess.view.Selected.Speaker) != nil {
			speaker = sess.view.Selected.Speaker
		} else if len(sess.view.Tracks) == 1 {
			speaker = sess.view.Tracks[0].Speaker
		}
	}
	if speaker == "" {
		if len(sess.view.Tracks) == 0 {
			return nil, core.Errorf(core.CodeIsolationBusy,
				"isolation %s is %s and has no isolated voice yet", sess.id, sess.view.Status)
		}
		return nil, core.Errorf(core.CodeInvalidRequest,
			"isolation %s has %d tracks; name the speaker", sess.id, len(sess.view.Tracks)).WithParam("speaker")
	}
	if t := sess.trackLocked(speaker); t != nil {
		return t, nil
	}
	if sess.voiceLocked(speaker) == nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "isolation %s has no voice %q", sess.id, speaker).
			WithParam("speaker")
	}
	return nil, core.Errorf(core.CodeIsolationBusy,
		"voice %s of isolation %s is not isolated yet", speaker, sess.id)
}

func (s *Service) Transcribe(ctx context.Context, id, speaker string, req core.Request) (*core.Job, error) {
	if s.opt.Jobs == nil {
		return nil, core.Errorf(core.CodeNotImplemented, "this server has no job queue")
	}
	f, track, err := s.openTrack(ctx, id, speaker)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	src := trackSource(sess, track)
	s.mu.Unlock()

	// The queue copies the audio into its own spool, so the job does not
	// depend on this isolation outliving it.
	req.Audio = src
	job, err := s.opt.Jobs.Submit(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if t := sess.trackLocked(track.Speaker); !sess.deleted && t != nil {
		t.JobID = job.ID
		s.changedLocked(sess)
		s.saveLocked(sess)
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
	if !ok || sess.deleted || (!caller.Admin && sess.caller.KeyID != caller.KeyID) {
		return nil, core.Errorf(core.CodeIsolationNotFound, "no such isolation: %s (it may have expired)", id)
	}
	return sess, nil
}

func (sess *session) voiceLocked(speaker string) *core.IsolationVoice {
	for i := range sess.view.Voices {
		if sess.view.Voices[i].Speaker == speaker {
			return &sess.view.Voices[i]
		}
	}
	return nil
}

func (sess *session) trackLocked(speaker string) *core.IsolationResult {
	for i := range sess.view.Tracks {
		if sess.view.Tracks[i].Speaker == speaker {
			return &sess.view.Tracks[i]
		}
	}
	return nil
}

// displayLocked is what a voice is called: its name, or its id.
func (sess *session) displayLocked(speaker string) string {
	if v := sess.voiceLocked(speaker); v != nil && v.Name != "" {
		return v.Name
	}
	return speaker
}

func (s *Service) viewLocked(sess *session) core.Isolation {
	v := sess.view
	v.Voices = slices.Clone(v.Voices)
	v.Warnings = slices.Clone(v.Warnings)
	v.Tracks = slices.Clone(v.Tracks)
	v.Result = nil
	if v.Selected != nil {
		sel := *v.Selected
		v.Selected = &sel
		if t := sess.trackLocked(sel.Speaker); t != nil {
			r := *t
			v.Result = &r
		}
	}
	if v.Transcript != nil {
		t := *v.Transcript
		t.Jobs = slices.Clone(t.Jobs)
		t.Warnings = slices.Clone(t.Warnings)
		v.Transcript = &t
	}
	if v.Keep {
		v.ExpiresAt = nil
	} else if v.ExpiresAt != nil {
		at := *v.ExpiresAt
		v.ExpiresAt = &at
	}
	if len(v.Tracks) > 0 {
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
	at := s.opt.Now().Add(s.opt.TTL)
	sess.view.ExpiresAt = &at
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
// never cut short; its time starts again when the work ends. One that is kept
// has no time.
func (s *Service) expire() {
	now := s.opt.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.view.Keep || sess.view.Status.Busy() || sess.view.ExpiresAt == nil {
			continue
		}
		if now.After(*sess.view.ExpiresAt) {
			s.opt.Log.Info("voice isolation expired", "isolation", sess.id)
			s.dropLocked(sess)
		}
	}
}

func addWarning(ws *[]core.Warning, w core.Warning) {
	for i := range *ws {
		if (*ws)[i].Code == w.Code {
			(*ws)[i] = w
			return
		}
	}
	*ws = append(*ws, w)
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

// resultName is what a track is called when it is downloaded or transcribed,
// without an extension: the upload's name with the voice's in it.
func resultName(upload, voice string) string {
	base := filepath.Base(upload)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	if base == "" || base == "." || base == "/" {
		base = "audio"
	}
	return base + "-" + fileSafe(voice)
}

// fileSafe keeps a voice's name usable in a file name: what a file system or
// a Content-Disposition header would choke on becomes an underscore.
func fileSafe(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, name)
}

var _ core.IsolationService = (*Service)(nil)
