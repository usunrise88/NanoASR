package isolation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
)

// Transcribing by speakers is diarization done the other way round. The
// diarizer cuts one recording into turns and labels each, so where two people
// speak at once one of them is lost and the other's words straddle the cut.
// Here every voice is isolated first and every track is transcribed alone:
// whose a word is follows from the track it came from, and two people
// speaking at once are both transcribed. It costs a transcription per voice.

func (s *Service) TranscribeSpeakers(ctx context.Context, id string, req core.Request) (*core.Isolation, error) {
	if s.opt.Jobs == nil {
		return nil, core.Errorf(core.CodeNotImplemented, "this server has no job queue")
	}
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
	// Each track is one voice: there is nobody to tell apart, and nothing
	// to split. What a client asked for besides is passed on as asked.
	req.Audio, req.Diarize, req.NumSpeakers = nil, false, 0
	req.ChannelMode, req.WebhookURL = "", ""
	sess.view.Transcript = &core.IsolationTranscript{
		Status:    core.JobQueued,
		Model:     req.ModelID,
		Language:  req.Language,
		CreatedAt: s.opt.Now(),
	}
	_ = os.Remove(filepath.Join(sess.dir, transcriptFile))
	s.startLocked(sess, task{id: id, kind: transcribe, voices: voices, req: req})
	v := s.viewLocked(sess)
	return &v, nil
}

// transcribeLocked submits every track and follows the jobs in the
// background. ctx is the session's: deleting the isolation, or stopping the
// server, cancels the jobs.
func (s *Service) transcribeLocked(ctx context.Context, sess *session, req core.Request) {
	tr := sess.view.Transcript
	if tr == nil {
		tr = &core.IsolationTranscript{CreatedAt: s.opt.Now()}
		sess.view.Transcript = tr
	}
	tr.Status, tr.Jobs, tr.Error, tr.Stale = core.JobRunning, nil, nil, false
	sess.view.Status, sess.view.Stage, sess.view.Voice, sess.view.Percent = core.IsolationTranscribing, "transcribe", "", 0
	var sources []trackJob
	for _, t := range sess.view.Tracks {
		sources = append(sources, trackJob{speaker: t.Speaker, src: trackSource(sess, t)})
	}
	s.wg.Add(1)
	go s.followJobs(ctx, sess, req, sources)
}

type trackJob struct {
	speaker string
	src     *resultSource
}

func (s *Service) followJobs(ctx context.Context, sess *session, req core.Request, tracks []trackJob) {
	defer s.wg.Done()
	// The jobs are the person's who made the isolation: theirs to see in
	// the history, and queued at their priority.
	jobCtx := core.WithCaller(ctx, sess.caller)
	var jobs []core.TranscriptJob
	result, err := func() (*core.Result, error) {
		if len(tracks) == 0 {
			return nil, core.Errorf(core.CodeInvalidRequest, "no voice has a track to transcribe")
		}
		for _, t := range tracks {
			r := req
			r.Audio = t.src
			job, err := s.opt.Jobs.Submit(jobCtx, r)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, core.TranscriptJob{Speaker: t.speaker, JobID: job.ID, Status: job.Status})
		}
		s.mu.Lock()
		if sess.view.Transcript != nil {
			sess.view.Transcript.Jobs = slices.Clone(jobs)
		}
		s.changedLocked(sess)
		s.saveLocked(sess)
		s.mu.Unlock()

		done := s.awaitJobs(jobCtx, sess, jobs)
		var parts []part
		for _, j := range done {
			if j.Status != core.JobSucceeded || j.Result == nil {
				msg := string(j.Status)
				if j.Error != nil {
					msg = j.Error.Message
				}
				s.mu.Lock()
				who := sess.displayLocked(j.speaker)
				s.mu.Unlock()
				return nil, core.Errorf(core.CodeInternal, "the transcription of %s did not succeed: %s", who, msg)
			}
			parts = append(parts, part{speaker: j.speaker, result: j.Result})
		}
		s.mu.Lock()
		voices := slices.Clone(sess.view.Voices)
		duration := sess.view.Duration
		s.mu.Unlock()
		return merge(sess.id, duration, voices, parts), nil
	}()

	if err != nil && ctx.Err() != nil {
		// Deleted, or the server is stopping: the jobs are nobody's now.
		for _, j := range jobs {
			_ = s.opt.Jobs.Cancel(context.WithoutCancel(jobCtx), j.JobID)
		}
		if s.ctx.Err() != nil {
			err = core.Errorf(core.CodeDraining, "the server stopped before the transcript was made")
		}
	}
	if err == nil {
		err = writeJSON(filepath.Join(sess.dir, transcriptFile), result)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.cancel != nil {
		sess.cancel()
		sess.cancel = nil
	}
	if sess.deleted {
		return
	}
	tr := sess.view.Transcript
	if err != nil {
		ce := core.AsError(err)
		if ce.Code == core.CodeInternal {
			s.opt.Log.Warn("transcript by speakers failed", "isolation", sess.id, "err", err)
		}
		if tr != nil {
			finishTranscript(tr, core.JobFailed, ce, s.opt.Now())
		}
	} else if tr != nil {
		finishTranscript(tr, core.JobSucceeded, nil, s.opt.Now())
		tr.Warnings = result.Warnings
	}
	// The tracks are as good as they were: a transcript that failed is not
	// an isolation that did.
	sess.view.Status, sess.view.Stage, sess.view.Voice, sess.view.Percent = core.IsolationDone, "", "", 100
	s.changedLocked(sess)
	s.saveLocked(sess)
}

func finishTranscript(tr *core.IsolationTranscript, status core.JobStatus, err *core.Error, now time.Time) {
	tr.Status, tr.Error = status, err
	tr.FinishedAt = &now
}

type finishedJob struct {
	speaker string
	*core.Job
}

// awaitJobs follows every job to its end, reporting their progress as the
// isolation's, and returns each as it finished, with its result.
func (s *Service) awaitJobs(ctx context.Context, sess *session, jobs []core.TranscriptJob) []finishedJob {
	out := make([]finishedJob, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = finishedJob{speaker: j.Speaker, Job: s.awaitJob(ctx, j.JobID, func(job core.Job) {
				s.jobProgress(sess, i, job)
			})}
		}()
	}
	wg.Wait()
	return out
}

func (s *Service) awaitJob(ctx context.Context, id string, progress func(core.Job)) *core.Job {
	gone := &core.Job{ID: id, Status: core.JobFailed, Error: core.Errorf(core.CodeDraining, "stopped")}
	var after int64
	for ctx.Err() == nil {
		events, err := s.opt.Jobs.Watch(ctx, id, after)
		if err != nil {
			return &core.Job{ID: id, Status: core.JobFailed, Error: core.AsError(err)}
		}
		for ev := range events {
			after = max(after, ev.Seq)
			progress(ev.Job)
			if terminal(ev.Job.Status) {
				// An event need not carry the result; the job does.
				job, err := s.opt.Jobs.Job(ctx, id)
				if err != nil {
					return &core.Job{ID: id, Status: core.JobFailed, Error: core.AsError(err)}
				}
				return job
			}
		}
		// The stream ended with the job still going: watch again, after a
		// breath, so a stream that keeps ending does not spin.
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	return gone
}

func terminal(s core.JobStatus) bool {
	switch s {
	case core.JobSucceeded, core.JobFailed, core.JobCanceled, core.JobExpired:
		return true
	}
	return false
}

func (s *Service) jobProgress(sess *session, i int, job core.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tr := sess.view.Transcript
	if sess.deleted || tr == nil || i >= len(tr.Jobs) {
		return
	}
	j := &tr.Jobs[i]
	j.Status, j.Percent = job.Status, job.Percent
	if job.Status == core.JobSucceeded {
		j.Percent = 100
	}
	total := 0
	for _, j := range tr.Jobs {
		total += j.Percent
	}
	sess.view.Percent = total / max(1, len(tr.Jobs))
	s.changedLocked(sess)
}

// part is one track's transcript.
type part struct {
	speaker string
	result  *core.Result
}

// merge makes one transcript of the tracks'. Every track is the whole
// recording with one voice left in it, so their timelines are the recording's
// and the segments interleave by time as they are.
func merge(id string, duration float64, voices []core.IsolationVoice, parts []part) *core.Result {
	out := &core.Result{
		ID:       id,
		Duration: duration,
		Stats:    core.Stats{AudioDuration: duration, StagesMS: map[string]int64{}},
		Segments: []core.Segment{},
	}
	order := map[string]int{}
	for i, v := range voices {
		order[v.Speaker] = i
	}
	totals := map[string]*core.Speaker{}
	seen := map[string]bool{}
	for _, p := range parts {
		r := p.result
		if out.Model == "" {
			out.Model, out.Language, out.TimestampSource = r.Model, r.Language, r.TimestampSource
		}
		out.Stats.ProcessingMS += r.Stats.ProcessingMS
		for stage, ms := range r.Stats.StagesMS {
			out.Stats.StagesMS[stage] += ms
		}
		for _, w := range r.Warnings {
			if !seen[w.Code] {
				seen[w.Code] = true
				out.Warnings = append(out.Warnings, w)
			}
		}
		sp := &core.Speaker{ID: p.speaker}
		totals[p.speaker] = sp
		for _, seg := range r.Segments {
			if strings.TrimSpace(seg.Text) == "" {
				continue
			}
			speaker := p.speaker
			seg.Speaker = &speaker
			seg.Channel = 0
			seg.Words = slices.Clone(seg.Words)
			for i := range seg.Words {
				seg.Words[i].Speaker = &speaker
				seg.Words[i].SpeakerConfidence = 0
				seg.Words[i].Channel = 0
			}
			out.Segments = append(out.Segments, seg)
			sp.Segments++
			sp.TotalSpeech += seg.End - seg.Start
		}
	}
	sort.SliceStable(out.Segments, func(i, j int) bool {
		a, b := out.Segments[i], out.Segments[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		return order[*a.Speaker] < order[*b.Speaker]
	})
	texts := make([]string, 0, len(out.Segments))
	for i := range out.Segments {
		out.Segments[i].ID = i
		texts = append(texts, strings.TrimSpace(out.Segments[i].Text))
	}
	out.Text = strings.Join(texts, " ")

	for _, v := range voices {
		if sp, ok := totals[v.Speaker]; ok {
			sp.TotalSpeech = round2(sp.TotalSpeech)
			out.Speakers = append(out.Speakers, *sp)
		}
	}
	var missing []string
	for _, v := range voices {
		if _, ok := totals[v.Speaker]; !ok {
			missing = append(missing, v.Speaker)
		}
	}
	if len(missing) > 0 {
		out.Warnings = append(out.Warnings, core.Warning{Code: "isolation_voices_missing",
			Message: fmt.Sprintf("not in the transcript, for want of a track: %s", strings.Join(missing, ", "))})
	}

	out.Silence, out.Stats.SpeechRatio = silences(out.Segments, duration)
	out.Stats.SegmentsTotal = len(out.Segments)
	if duration > 0 {
		out.Stats.RTF = float64(out.Stats.ProcessingMS) / 1000 / duration
	}
	return out
}

// silences is where nobody speaks: the gaps between the segments of every
// voice taken together.
func silences(segs []core.Segment, duration float64) ([]core.Silence, float64) {
	out := []core.Silence{}
	at, speech := 0.0, 0.0
	for _, s := range segs {
		if s.Start > at {
			out = append(out, core.Silence{Start: round2(at), End: round2(s.Start)})
		}
		if s.End > at {
			speech += s.End - max(at, s.Start)
			at = s.End
		}
	}
	if duration > at {
		out = append(out, core.Silence{Start: round2(at), End: round2(duration)})
	}
	if duration <= 0 {
		return out, 0
	}
	return out, round2(min(1, speech/duration))
}

func (s *Service) Transcript(ctx context.Context, id string) (*core.Result, error) {
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	tr := sess.view.Transcript
	if tr == nil || tr.Status != core.JobSucceeded {
		status := "not been asked for"
		if tr != nil {
			status = string(tr.Status)
		}
		s.mu.Unlock()
		return nil, core.Errorf(core.CodeIsolationBusy,
			"isolation %s has no transcript by speakers: it has %s", id, status)
	}
	path := filepath.Join(sess.dir, transcriptFile)
	names := map[string]string{}
	for _, v := range sess.view.Voices {
		names[v.Speaker] = v.Name
	}
	s.mu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot read the transcript").WithCause(err)
	}
	var res core.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot read the transcript").WithCause(err)
	}
	// Names are read now rather than written into the transcript: a voice
	// renamed after it was made is called by its new name.
	for i := range res.Speakers {
		res.Speakers[i].Name = names[res.Speakers[i].ID]
	}
	return &res, nil
}

// trackSource hands a track to the job queue as an upload.
func trackSource(sess *session, t core.IsolationResult) *resultSource {
	path := filepath.Join(sess.dir, trackFile(t.Speaker))
	size := int64(audio.WAVHeaderSize)
	if info, err := os.Stat(path); err == nil {
		size += info.Size()
	}
	return &resultSource{path: path, name: t.Name + ".wav", size: size}
}
