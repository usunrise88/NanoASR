package isolation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/tse"
)

// A kept isolation is its directory: the audio, and state.json beside it with
// everything that is otherwise only in memory. The state is written whole
// after every step that changes what a person would find, and its presence is
// what keeping means — an isolation without one is deleted at startup.

// stateVersion changes when state.json stops being readable by older code.
const stateVersion = 1

type state struct {
	Version     int                    `json:"version"`
	Caller      savedCaller            `json:"caller"`
	NumSpeakers int                    `json:"num_speakers,omitempty"`
	Isolation   core.Isolation         `json:"isolation"`
	Speakers    []savedSpeaker         `json:"speakers,omitempty"`
	Samples     map[string]savedSample `json:"samples,omitempty"`
}

type savedCaller struct {
	KeyID       string `json:"key_id,omitempty"`
	Admin       bool   `json:"admin,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
}

type savedSpeaker struct {
	ID    string       `json:"id"`
	Spans [][2]float64 `json:"spans"`
}

// savedSample is a sample without its audio, which is in sample-<id>.s16.
type savedSample struct {
	Kind        tse.SampleKind `json:"kind"`
	Start       float64        `json:"start,omitempty"`
	Similarity  float64        `json:"similarity"`
	Embedding   []float32      `json:"embedding"`
	CleanSpeech float64        `json:"clean_speech,omitempty"`
}

// saveLocked writes a kept isolation's state; one that is not kept has none.
// A failure is logged and not returned: the isolation goes on working, and
// what is lost is only what a restart would have brought back.
func (s *Service) saveLocked(sess *session) {
	if !sess.view.Keep || sess.deleted {
		return
	}
	st := state{
		Version:     stateVersion,
		Caller:      savedCaller{KeyID: sess.caller.KeyID, Admin: sess.caller.Admin, Interactive: sess.caller.Interactive},
		NumSpeakers: sess.numSpeakers,
		Isolation:   s.viewLocked(sess),
		Samples:     map[string]savedSample{},
	}
	st.Isolation.Position, st.Isolation.Formats = 0, nil
	for _, sp := range sess.speakers {
		saved := savedSpeaker{ID: sp.ID}
		for _, span := range sp.Spans {
			saved.Spans = append(saved.Spans, [2]float64{span.Start, span.End})
		}
		st.Speakers = append(st.Speakers, saved)
	}
	for id, smp := range sess.samples {
		st.Samples[id] = savedSample{Kind: smp.Kind, Start: smp.Start, Similarity: smp.Similarity,
			Embedding: smp.Embedding, CleanSpeech: smp.CleanSpeech}
	}
	if err := writeJSON(filepath.Join(sess.dir, stateFile), st); err != nil {
		s.opt.Log.Error("cannot save a kept voice isolation", "isolation", sess.id, "err", err)
	}
}

// load brings back the kept isolations and deletes everything else under
// Dir: what is not kept belonged to a process that is gone, and so did the
// person waiting for it.
func (s *Service) load() error {
	entries, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return fmt.Errorf("isolation: cannot read %s: %w", s.opt.Dir, err)
	}
	for _, e := range entries {
		path := filepath.Join(s.opt.Dir, e.Name())
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "iso_") {
			_ = os.RemoveAll(path)
			continue
		}
		b, err := os.ReadFile(filepath.Join(path, stateFile))
		if os.IsNotExist(err) {
			_ = os.RemoveAll(path)
			continue
		}
		sess, err := s.restore(path, b)
		if err != nil {
			// Somebody asked for it to be kept: an unreadable state is
			// reported and left alone, not deleted.
			s.opt.Log.Error("cannot load a kept voice isolation; leaving it on disk",
				"dir", path, "err", err)
			continue
		}
		s.sessions[sess.id] = sess
	}
	if n := len(s.sessions); n > 0 {
		s.opt.Log.Info("kept voice isolations loaded", "count", n)
	}
	return nil
}

func (s *Service) restore(dir string, b []byte) (*session, error) {
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Version != stateVersion {
		return nil, fmt.Errorf("state version %d, this server reads %d", st.Version, stateVersion)
	}
	if st.Isolation.ID != filepath.Base(dir) {
		return nil, fmt.Errorf("the state is of %q", st.Isolation.ID)
	}
	sess := &session{
		id:          st.Isolation.ID,
		caller:      core.Caller{KeyID: st.Caller.KeyID, Admin: st.Caller.Admin, Interactive: st.Caller.Interactive},
		dir:         dir,
		view:        st.Isolation,
		numSpeakers: st.NumSpeakers,
		samples:     map[string]tse.Sample{},
		subs:        map[chan struct{}]struct{}{},
		seq:         1,
	}
	sess.view.Keep = true
	for _, sp := range st.Speakers {
		speaker := tse.Speaker{ID: sp.ID}
		for _, span := range sp.Spans {
			speaker.Spans = append(speaker.Spans, tse.Span{Start: span[0], End: span[1]})
		}
		sess.speakers = append(sess.speakers, speaker)
	}
	for id, smp := range st.Samples {
		if sess.voiceLocked(id) == nil {
			return nil, fmt.Errorf("a sample of an unknown voice %q", id)
		}
		a, err := readPCM(filepath.Join(dir, sampleFile(id)))
		if err != nil {
			return nil, err
		}
		sess.samples[id] = tse.Sample{Kind: smp.Kind, Start: smp.Start, Audio: a, Similarity: smp.Similarity,
			Embedding: smp.Embedding, CleanSpeech: smp.CleanSpeech}
	}
	// A track whose file is gone is not a track.
	tracks := sess.view.Tracks[:0:0]
	for _, t := range sess.view.Tracks {
		if _, err := os.Stat(filepath.Join(dir, trackFile(t.Speaker))); err == nil {
			tracks = append(tracks, t)
		}
	}
	sess.view.Tracks = tracks

	// Work the server was doing when it stopped did not finish, and is not
	// started again on its own: the person who asked may no longer want it.
	if sess.view.Status.Busy() {
		interrupted := core.Errorf(core.CodeDraining, "the server restarted before this finished; start it again")
		switch {
		case len(sess.view.Voices) == 0 && fileExists(filepath.Join(dir, uploadFile)):
			sess.view.Status = core.IsolationQueued
			s.pending = append(s.pending, task{id: sess.id, kind: analyse})
		case len(sess.view.Voices) == 0:
			sess.view.Status, sess.view.Error = core.IsolationFailed, interrupted
		case len(sess.view.Tracks) > 0:
			sess.view.Status = core.IsolationDone
		default:
			sess.view.Status = core.IsolationAwaitingSelection
		}
		sess.view.Stage, sess.view.Voice, sess.view.Percent = "", "", 0
		if tr := sess.view.Transcript; tr != nil && (tr.Status == core.JobQueued || tr.Status == core.JobRunning) {
			finishTranscript(tr, core.JobFailed, interrupted, s.opt.Now())
		}
	}
	return sess, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
