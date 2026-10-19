package isolation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/tse"
)

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

	err := s.work(ctx, sess, t)

	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.deleted {
		cancel()
		return
	}
	if err == nil && t.kind == transcribe {
		// The tracks are made; the jobs are the queue's work, not this
		// worker's, so the worker is free for the next isolation while
		// they run. The cancel stays with the session to stop them.
		s.transcribeLocked(ctx, sess, t.req)
		s.changedLocked(sess)
		s.saveLocked(sess)
		return
	}
	sess.cancel = nil
	cancel()
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
		sess.view.Stage, sess.view.Voice = "", ""
		if t.kind == transcribe && sess.view.Transcript != nil {
			finishTranscript(sess.view.Transcript, core.JobFailed, ce, s.opt.Now())
		}
	}
	s.changedLocked(sess)
	s.saveLocked(sess)
}

// work does the part of a task that runs on this server's threads.
func (s *Service) work(ctx context.Context, sess *session, t task) error {
	if err := s.opt.Governor.Acquire(ctx, s.opt.Threads); err != nil {
		return err
	}
	defer s.opt.Governor.Release(s.opt.Threads)
	switch t.kind {
	case analyse:
		return s.analyse(ctx, sess)
	case transcribe:
		// Not done when the tracks are: the jobs come next, and a watcher
		// that saw "done" in between would stop watching.
		return s.extract(ctx, sess, t.voices, false)
	default:
		return s.extract(ctx, sess, t.voices, true)
	}
}

// stage reports progress; a percentage that has not moved is not news.
func (s *Service) stage(sess *session, status core.IsolationStatus, stage, voice string, percent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.deleted {
		return
	}
	v := &sess.view
	if v.Status == status && v.Stage == stage && v.Voice == voice && v.Percent == percent {
		return
	}
	v.Status, v.Stage, v.Voice, v.Percent = status, stage, voice, percent
	s.changedLocked(sess)
}

func (s *Service) analyse(ctx context.Context, sess *session) error {
	s.stage(sess, core.IsolationAnalysing, "decode", "", 0)
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
	if len(x) == 0 {
		return core.Errorf(core.CodeInvalidRequest, "the file contains no audio")
	}
	if err := writePCM(filepath.Join(sess.dir, sourceFile), x); err != nil {
		return err
	}
	// The decoded copy is what everything after reads, and what a kept
	// isolation plays; the upload, in whatever format it came, is not.
	_ = os.Remove(upload)
	dur := float64(len(x)) / SampleRate
	s.mu.Lock()
	sess.view.Duration = round2(dur)
	s.mu.Unlock()

	s.stage(sess, core.IsolationAnalysing, "diarize", "", 10)
	turns, err := s.opt.Diarizer.Process(ctx, audio.PCM{Samples: x, SampleRate: SampleRate, SourceChannels: 1}, sess.numSpeakers)
	if err != nil {
		return err
	}
	speakers := speakersOf(turns)
	if len(speakers) == 0 {
		return core.Errorf(core.CodeInvalidRequest, "no voices were found in the recording")
	}

	s.stage(sess, core.IsolationAnalysing, "samples", "", 60)
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
			// On disk for when the isolation is kept: a restart must not
			// need the models to find the samples again.
			if err := writePCM(filepath.Join(sess.dir, sampleFile(v.Speaker.ID)), v.Sample.Audio); err != nil {
				return err
			}
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

// extract isolates the voices one after another, each on a track of its own.
// A track is published as soon as it is made, so a client watching sees the
// voices arrive one by one. finish marks the isolation done afterwards.
func (s *Service) extract(ctx context.Context, sess *session, voices []voiceTask, finish bool) error {
	if len(voices) == 0 {
		return nil
	}
	s.stage(sess, core.IsolationExtracting, "extract", voices[0].speaker, 0)
	x, err := readPCM(filepath.Join(sess.dir, sourceFile))
	if err != nil {
		return err
	}
	eng, err := s.opt.Engine(ctx)
	if err != nil {
		return err
	}
	for i, vt := range voices {
		s.mu.Lock()
		var keep []tse.Span
		for _, sp := range sess.speakers {
			if sp.ID == vt.speaker {
				keep = sp.Spans
			}
		}
		sample, haveSample := sess.samples[vt.speaker]
		var others [][]float32
		for id, smp := range sess.samples {
			if id != vt.speaker {
				others = append(others, smp.Embedding)
			}
		}
		s.mu.Unlock()

		if vt.sampleStart != nil || !haveSample {
			start := 0.0
			if vt.sampleStart != nil {
				start = *vt.sampleStart
			}
			sample, err = tse.SampleAt(x, start, embedderFunc(eng.Embed))
			if err != nil {
				return err
			}
		}
		n := float64(len(voices))
		y, err := eng.Extract(ctx, x, sample, others, keep, func(f float64) {
			s.stage(sess, core.IsolationExtracting, "extract", vt.speaker, int((float64(i)+f)/n*99))
		})
		if err != nil {
			return err
		}
		if err := writePCM(filepath.Join(sess.dir, trackFile(vt.speaker)), y); err != nil {
			return err
		}

		s.mu.Lock()
		if sess.deleted {
			s.mu.Unlock()
			return nil
		}
		track := core.IsolationResult{
			Speaker:     vt.speaker,
			Name:        resultName(sess.view.Filename, sess.displayLocked(vt.speaker)),
			Duration:    round2(float64(len(y)) / SampleRate),
			SampleRate:  SampleRate,
			SampleStart: vt.sampleStart,
		}
		s.putTrackLocked(sess, track)
		s.changedLocked(sess)
		s.saveLocked(sess)
		s.mu.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if finish && !sess.deleted {
		sess.view.Status, sess.view.Stage, sess.view.Voice, sess.view.Percent = core.IsolationDone, "", "", 100
	}
	return nil
}

// putTrackLocked adds a track in the order of the voices, replacing the
// voice's previous one.
func (s *Service) putTrackLocked(sess *session, track core.IsolationResult) {
	order := map[string]int{}
	for i, v := range sess.view.Voices {
		order[v.Speaker] = i
	}
	tracks := sess.view.Tracks[:0:0]
	for _, t := range sess.view.Tracks {
		if t.Speaker != track.Speaker {
			tracks = append(tracks, t)
		}
	}
	tracks = append(tracks, track)
	sort.SliceStable(tracks, func(i, j int) bool { return order[tracks[i].Speaker] < order[tracks[j].Speaker] })
	sess.view.Tracks = tracks
}
