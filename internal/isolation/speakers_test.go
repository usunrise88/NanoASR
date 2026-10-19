package isolation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

func seg(start, end float64, text string) core.Segment {
	return core.Segment{Start: start, End: end, Text: text,
		Words: []core.Word{{Word: text, Start: start, End: end, Confidence: 0.9}}}
}

// analysed creates an isolation and waits for its voices.
func (f *fixture) analysed(t *testing.T, ctx context.Context, req core.IsolationRequest) core.Isolation {
	t.Helper()
	if req.Audio == nil {
		req.Audio = upload{wavBytes(10)}
	}
	iso, err := f.svc.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	got := f.wait(t, ctx, iso.ID)
	if got.Status != core.IsolationAwaitingSelection {
		t.Fatalf("after analysis: %s %+v", got.Status, got.Error)
	}
	return got
}

func TestEveryVoiceOnATrackOfItsOwn(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	iso := f.analysed(t, ctx, core.IsolationRequest{})

	if _, err := f.svc.ExtractAll(ctx, iso.ID); err != nil {
		t.Fatal(err)
	}
	got := f.wait(t, ctx, iso.ID)
	if got.Status != core.IsolationDone || len(got.Tracks) != 2 {
		t.Fatalf("after extracting all: %s, tracks %+v, %+v", got.Status, got.Tracks, got.Error)
	}
	// In the order of the voices; the one without a sample is skipped and
	// said to be.
	if got.Tracks[0].Speaker != "spk_1" || got.Tracks[1].Speaker != "spk_0" || got.Tracks[1].Name != "call-spk_0" {
		t.Errorf("tracks %+v", got.Tracks)
	}
	if len(got.Warnings) != 1 || got.Warnings[0].Code != "isolation_voices_skipped" {
		t.Errorf("warnings %+v", got.Warnings)
	}
	if len(f.eng.extracted) != 2 {
		t.Errorf("%d extractions", len(f.eng.extracted))
	}
	for _, sp := range []string{"spk_1", "spk_0"} {
		rc, err := f.svc.Result(ctx, iso.ID, sp, core.AudioWAV)
		if err != nil {
			t.Fatalf("track %s: %v", sp, err)
		}
		_ = rc.Close()
	}
	// With two tracks and none chosen, a track must be named.
	if _, err := f.svc.Result(ctx, iso.ID, "", core.AudioWAV); core.AsError(err).Code != core.CodeInvalidRequest {
		t.Errorf("an unnamed track among two: %v", err)
	}
	if _, err := f.svc.Result(ctx, iso.ID, "spk_2", core.AudioWAV); core.AsError(err).Code != core.CodeIsolationBusy {
		t.Errorf("a voice not isolated: %v", err)
	}

	// Asking again does nothing: every voice that can have a track has one.
	if again, err := f.svc.ExtractAll(ctx, iso.ID); err != nil || again.Status != core.IsolationDone {
		t.Errorf("extract all again: %v %+v", err, again)
	}
	if len(f.eng.extracted) != 2 {
		t.Errorf("extracted again: %d", len(f.eng.extracted))
	}

	// Isolating one voice again replaces its track and leaves the other.
	start := 1.0
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0", SampleStart: &start}); err != nil {
		t.Fatal(err)
	}
	got = f.wait(t, ctx, iso.ID)
	if len(got.Tracks) != 2 || got.Tracks[1].SampleStart == nil || got.Tracks[0].SampleStart != nil {
		t.Errorf("after isolating one again: %+v", got.Tracks)
	}
}

func TestVoicesAreNamed(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	iso := f.analysed(t, ctx, core.IsolationRequest{})
	if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_1"}); err != nil {
		t.Fatal(err)
	}
	f.wait(t, ctx, iso.ID)

	got, err := f.svc.Update(ctx, iso.ID, core.IsolationUpdate{Names: map[string]string{"spk_1": "  Анна/К \n"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Voices[0].Name != "Анна/К" {
		t.Errorf("name %q", got.Voices[0].Name)
	}
	// The track takes the name, made safe for a file.
	if got.Tracks[0].Name != "call-Анна_К" {
		t.Errorf("track name %q", got.Tracks[0].Name)
	}
	if _, err := f.svc.Transcribe(ctx, iso.ID, "spk_1", core.Request{}); err != nil {
		t.Fatal(err)
	}
	if name := f.jobs.submits[0].Audio.Filename(); name != "call-Анна_К.wav" {
		t.Errorf("the job's file %q", name)
	}

	// Nothing changes when any of it is wrong.
	long := strings.Repeat("я", core.MaxSpeakerName+1)
	for _, names := range []map[string]string{
		{"spk_9": "Борис"},
		{"spk_0": "Борис", "spk_1": long},
	} {
		if _, err := f.svc.Update(ctx, iso.ID, core.IsolationUpdate{Names: names}); core.AsError(err).Code != core.CodeInvalidRequest {
			t.Errorf("%v: %v", names, err)
		}
	}
	if v, _ := f.svc.Isolation(ctx, iso.ID); v.Voices[1].Name != "" {
		t.Errorf("half an update applied: %+v", v.Voices[1])
	}
	// An empty name gives the voice its id back.
	got, err = f.svc.Update(ctx, iso.ID, core.IsolationUpdate{Names: map[string]string{"spk_1": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Voices[0].Name != "" || got.Tracks[0].Name != "call-spk_1" {
		t.Errorf("after clearing: %+v %+v", got.Voices[0], got.Tracks[0])
	}
}

func TestTranscriptBySpeakers(t *testing.T) {
	f := newFixture(t, nil)
	f.jobs.segments = map[string][]core.Segment{
		// spk_0 speaks first and once more over spk_1's turn.
		"call-spk_0.wav": {seg(0.5, 3.5, "привет"), seg(6, 7, "да-да"), seg(8, 8.2, " ")},
		"call-Анна.wav":  {seg(4, 6.5, "здравствуйте"), seg(7.5, 9, "как дела")},
	}
	alice := core.WithCaller(context.Background(), core.Caller{KeyID: "alice", Interactive: true})
	iso := f.analysed(t, alice, core.IsolationRequest{})
	if _, err := f.svc.Update(alice, iso.ID, core.IsolationUpdate{Names: map[string]string{"spk_1": "Анна"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Transcript(alice, iso.ID); core.AsError(err).Code != core.CodeIsolationBusy {
		t.Errorf("a transcript nobody asked for: %v", err)
	}

	started, err := f.svc.TranscribeSpeakers(alice, iso.ID, core.Request{ModelID: "gigaam", Diarize: true, NumSpeakers: 2})
	if err != nil {
		t.Fatal(err)
	}
	// One watch follows it to the end: nothing in between reads as done.
	var statuses []core.IsolationStatus
	ch, err := f.svc.Watch(alice, iso.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		statuses = append(statuses, ev.Isolation.Status)
	}
	for i, st := range statuses {
		if !st.Busy() && i != len(statuses)-1 {
			t.Errorf("the watch saw %s before the end: %v", st, statuses)
		}
	}
	if last := statuses[len(statuses)-1]; last != core.IsolationDone {
		t.Errorf("the watch ended at %s: %v", last, statuses)
	}
	if started.Transcript == nil || started.Transcript.Status != core.JobQueued || started.Transcript.Model != "gigaam" {
		t.Fatalf("started %+v", started.Transcript)
	}
	got := f.wait(t, alice, iso.ID)
	tr := got.Transcript
	if got.Status != core.IsolationDone || tr == nil || tr.Status != core.JobSucceeded || tr.FinishedAt == nil {
		t.Fatalf("after transcribing: %s %+v %+v", got.Status, tr, got.Error)
	}
	// Every voice that can be isolated was, and each track is a job of the
	// caller's, without diarization.
	if len(got.Tracks) != 2 || len(f.jobs.submits) != 2 || len(tr.Jobs) != 2 {
		t.Fatalf("tracks %d, submits %d, jobs %+v", len(got.Tracks), len(f.jobs.submits), tr.Jobs)
	}
	for i, req := range f.jobs.submits {
		if req.Diarize || req.NumSpeakers != 0 || req.ModelID != "gigaam" || f.jobs.callers[i].KeyID != "alice" {
			t.Errorf("job %d: %+v by %+v", i, req, f.jobs.callers[i])
		}
	}
	if tr.Jobs[0].Speaker != "spk_1" || tr.Jobs[0].Status != core.JobSucceeded || tr.Jobs[0].Percent != 100 {
		t.Errorf("jobs %+v", tr.Jobs)
	}

	res, err := f.svc.Transcript(alice, iso.ID)
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, s := range res.Segments {
		said = append(said, *s.Speaker+":"+s.Text)
		if *s.Words[0].Speaker != *s.Speaker {
			t.Errorf("a word of %s labelled %s", *s.Speaker, *s.Words[0].Speaker)
		}
	}
	want := []string{"spk_0:привет", "spk_1:здравствуйте", "spk_0:да-да", "spk_1:как дела"}
	if len(said) != len(want) {
		t.Fatalf("segments %v", said)
	}
	for i := range want {
		if said[i] != want[i] || res.Segments[i].ID != i {
			t.Errorf("segment %d: %s, want %s", i, said[i], want[i])
		}
	}
	if res.Text != "привет здравствуйте да-да как дела" || res.Model != "gigaam" || res.Duration != 10 {
		t.Errorf("text %q, model %q, duration %g", res.Text, res.Model, res.Duration)
	}
	// The voices by their names, the most talkative first as everywhere.
	if len(res.Speakers) != 2 || res.Speakers[0].Name != "Анна" || res.Speakers[0].TotalSpeech != 4 ||
		res.Speakers[1].Segments != 2 {
		t.Errorf("speakers %+v", res.Speakers)
	}
	// Silence is where neither speaks; their overlap is speech once.
	if len(res.Silence) != 4 || res.Silence[1] != (core.Silence{Start: 3.5, End: 4}) ||
		res.Silence[2] != (core.Silence{Start: 7, End: 7.5}) || res.Stats.SpeechRatio != 0.75 {
		t.Errorf("silence %+v, speech %g", res.Silence, res.Stats.SpeechRatio)
	}
	if res.Stats.ProcessingMS != 200 || res.Stats.StagesMS["asr"] != 160 ||
		len(res.Warnings) != 2 || res.Warnings[1].Code != "isolation_voices_missing" {
		t.Errorf("stats %+v, warnings %+v", res.Stats, res.Warnings)
	}

	// A rename reaches a transcript already made.
	if _, err := f.svc.Update(alice, iso.ID, core.IsolationUpdate{Names: map[string]string{"spk_0": "Борис"}}); err != nil {
		t.Fatal(err)
	}
	if res, _ = f.svc.Transcript(alice, iso.ID); res.Speakers[1].Name != "Борис" {
		t.Errorf("after a rename: %+v", res.Speakers)
	}

	// Isolating a voice again leaves the transcript, marked out of date.
	if _, err := f.svc.Extract(alice, iso.ID, core.IsolationSelection{Speaker: "spk_0"}); err != nil {
		t.Fatal(err)
	}
	got = f.wait(t, alice, iso.ID)
	if got.Transcript == nil || !got.Transcript.Stale {
		t.Errorf("transcript after a track changed: %+v", got.Transcript)
	}
	if _, err := f.svc.Transcript(alice, iso.ID); err != nil {
		t.Errorf("a stale transcript is still a transcript: %v", err)
	}
}

func TestAFailedTranscriptLeavesTheTracks(t *testing.T) {
	f := newFixture(t, nil)
	f.jobs.fail = "call-spk_0.wav"
	ctx := context.Background()
	iso := f.analysed(t, ctx, core.IsolationRequest{})
	if _, err := f.svc.TranscribeSpeakers(ctx, iso.ID, core.Request{}); err != nil {
		t.Fatal(err)
	}
	got := f.wait(t, ctx, iso.ID)
	if got.Status != core.IsolationDone || len(got.Tracks) != 2 {
		t.Fatalf("after a failed transcript: %s, %d tracks", got.Status, len(got.Tracks))
	}
	if tr := got.Transcript; tr.Status != core.JobFailed || tr.Error == nil {
		t.Errorf("transcript %+v", tr)
	}
	if _, err := f.svc.Transcript(ctx, iso.ID); core.AsError(err).Code != core.CodeIsolationBusy {
		t.Errorf("the transcript of a failure: %v", err)
	}
}

func TestDeletingStopsTheTranscriptJobs(t *testing.T) {
	f := newFixture(t, nil)
	f.jobs.hold = make(chan struct{})
	defer close(f.jobs.hold)
	ctx := context.Background()
	iso := f.analysed(t, ctx, core.IsolationRequest{})
	if _, err := f.svc.TranscribeSpeakers(ctx, iso.ID, core.Request{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, _ := f.svc.Isolation(ctx, iso.ID)
		if v.Status == core.IsolationTranscribing && len(v.Transcript.Jobs) == 2 {
			// Nothing can be chosen while the jobs run.
			if _, err := f.svc.Extract(ctx, iso.ID, core.IsolationSelection{Speaker: "spk_0"}); core.AsError(err).Code != core.CodeIsolationBusy {
				t.Errorf("extract while transcribing: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never transcribing: %+v", v)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := f.svc.Delete(ctx, iso.ID); err != nil {
		t.Fatal(err)
	}
	for {
		f.jobs.mu.Lock()
		n := len(f.jobs.canceled)
		f.jobs.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d jobs canceled", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAKeptIsolationOutlivesTheServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "isolations")
	f := newFixture(t, func(o *Options) { o.Dir, o.TTL = dir, time.Minute })
	f.jobs.segments = map[string][]core.Segment{"call-spk_1.wav": {seg(4, 6, "здравствуйте")}}
	ctx := context.Background()
	kept := f.analysed(t, ctx, core.IsolationRequest{Keep: true})
	if !kept.Keep || kept.ExpiresAt != nil {
		t.Fatalf("kept %v, expires %v", kept.Keep, kept.ExpiresAt)
	}
	gone := f.analysed(t, ctx, core.IsolationRequest{})
	if _, err := f.svc.Update(ctx, kept.ID, core.IsolationUpdate{Names: map[string]string{"spk_1": "Анна"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.TranscribeSpeakers(ctx, kept.ID, core.Request{ModelID: "gigaam"}); err != nil {
		t.Fatal(err)
	}
	before := f.wait(t, ctx, kept.ID)

	// Time passes: what is kept has none to run out of.
	*f.clock = f.clock.Add(time.Hour)
	f.svc.expire()
	if _, err := f.svc.Isolation(ctx, kept.ID); err != nil {
		t.Fatalf("a kept isolation expired: %v", err)
	}

	// A restart: the other isolation is gone, this one is back as it was.
	f.svc.Close()
	g := newFixture(t, func(o *Options) { o.Dir = dir })
	after, err := g.svc.Isolation(ctx, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != core.IsolationDone || len(after.Tracks) != 2 || after.Voices[0].Name != "Анна" ||
		after.Transcript == nil || after.Transcript.Status != core.JobSucceeded || !after.Keep {
		t.Fatalf("after a restart: %+v", after)
	}
	if after.Duration != before.Duration || after.CreatedAt != before.CreatedAt {
		t.Errorf("before %+v, after %+v", before, after)
	}
	if _, err := g.svc.Isolation(ctx, gone.ID); core.AsError(err).Code != core.CodeIsolationNotFound {
		t.Errorf("an isolation not kept survived a restart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, gone.ID)); !os.IsNotExist(err) {
		t.Errorf("its audio survived: %v", err)
	}
	if res, err := g.svc.Transcript(ctx, kept.ID); err != nil || res.Speakers[0].Name != "Анна" {
		t.Errorf("transcript after a restart: %v %+v", err, res)
	}
	for _, read := range []func() error{
		func() error { r, err := g.svc.Sample(ctx, kept.ID, "spk_1"); closeIf(r); return err },
		func() error { r, err := g.svc.Result(ctx, kept.ID, "spk_0", core.AudioWAV); closeIf(r); return err },
		func() error { r, err := g.svc.Audio(ctx, kept.ID, core.AudioWAV); closeIf(r); return err },
	} {
		if err := read(); err != nil {
			t.Errorf("audio after a restart: %v", err)
		}
	}
	// The samples came back with their embeddings: another voice can be
	// isolated without the models finding them again.
	if _, err := g.svc.Extract(ctx, kept.ID, core.IsolationSelection{Speaker: "spk_1"}); err != nil {
		t.Fatal(err)
	}
	g.wait(t, ctx, kept.ID)
	if others := g.eng.others[0]; len(others) != 1 || others[0][0] != 1 {
		t.Errorf("others after a restart %v", others)
	}

	// Released, it expires as any other.
	keep := false
	if _, err := g.svc.Update(ctx, kept.ID, core.IsolationUpdate{Keep: &keep}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, kept.ID, stateFile)); !os.IsNotExist(err) {
		t.Errorf("released but still kept: %v", err)
	}
	*g.clock = g.clock.Add(2 * time.Hour)
	g.svc.expire()
	if _, err := g.svc.Isolation(ctx, kept.ID); core.AsError(err).Code != core.CodeIsolationNotFound {
		t.Errorf("released and never expired: %v", err)
	}
}

func closeIf(r interface{ Close() error }) {
	if r != nil {
		_ = r.Close()
	}
}

func TestWorkCutShortByARestartIsNotResumed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "isolations")
	f := newFixture(t, func(o *Options) { o.Dir = dir })
	f.jobs.hold = make(chan struct{})
	defer close(f.jobs.hold)
	ctx := context.Background()
	kept := f.analysed(t, ctx, core.IsolationRequest{Keep: true})
	if _, err := f.svc.TranscribeSpeakers(ctx, kept.ID, core.Request{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if v, _ := f.svc.Isolation(ctx, kept.ID); v.Status == core.IsolationTranscribing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never transcribing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.svc.Close()

	g := newFixture(t, func(o *Options) { o.Dir = dir })
	after, err := g.svc.Isolation(ctx, kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != core.IsolationDone || after.Transcript.Status != core.JobFailed ||
		after.Transcript.Error.Code != core.CodeDraining {
		t.Errorf("after a restart mid-transcript: %s %+v", after.Status, after.Transcript)
	}
}

func TestKeepingCanBeSwitchedOff(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.AllowKeep = false })
	ctx := context.Background()
	if _, err := f.svc.Create(ctx, core.IsolationRequest{Audio: upload{wavBytes(10)}, Keep: true}); core.AsError(err).Code != core.CodeCapabilityUnavailable {
		t.Errorf("keep where it is off: %v", err)
	}
	iso := f.analysed(t, ctx, core.IsolationRequest{})
	keep := true
	if _, err := f.svc.Update(ctx, iso.ID, core.IsolationUpdate{Keep: &keep}); core.AsError(err).Code != core.CodeCapabilityUnavailable {
		t.Errorf("keep later where it is off: %v", err)
	}
}

func TestTheListIsTheCallersNewestFirst(t *testing.T) {
	var tick atomic.Int64
	f := newFixture(t, func(o *Options) {
		start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
		o.Now = func() time.Time { return start.Add(time.Duration(tick.Add(1)) * time.Second) }
	})
	alice := core.WithCaller(context.Background(), core.Caller{KeyID: "alice"})
	bob := core.WithCaller(context.Background(), core.Caller{KeyID: "bob"})
	first, _ := f.svc.Create(alice, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	second, _ := f.svc.Create(alice, core.IsolationRequest{Audio: upload{wavBytes(10)}})
	if _, err := f.svc.Create(bob, core.IsolationRequest{Audio: upload{wavBytes(10)}}); err != nil {
		t.Fatal(err)
	}
	list, err := f.svc.List(alice)
	if err != nil || len(list) != 2 || list[0].ID != second.ID || list[1].ID != first.ID {
		t.Errorf("alice sees %+v (%v)", list, err)
	}
	if all, _ := f.svc.List(context.Background()); len(all) != 3 {
		t.Errorf("an administrator sees %d", len(all))
	}
	for _, id := range []string{first.ID, second.ID} {
		f.wait(t, alice, id)
	}
}
