package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/job"
	"github.com/usunrise88/nanoasr/internal/store/sqlite"
)

// renameStack is a pipeline with a real store and an idle queue: renaming
// touches only what the store keeps, and needs no model.
func renameStack(t *testing.T, recs ...job.Record) *Pipeline {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	p := &Pipeline{}
	p.Attach(job.New(store, p, job.Options{Size: 1, Workers: 1, Hub: job.NewHub(4)}), store)
	ctx := context.Background()
	for _, r := range recs {
		if err := store.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := store.Update(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func diarized(id, key string, status core.JobStatus) job.Record {
	a, b := "spk_0", "spk_1"
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := job.Record{
		APIKeyID: key,
		Job:      core.Job{ID: id, Status: status, ModelID: "m", CreatedAt: now, FinishedAt: &now, Source: core.SourceAPI},
	}
	if status == core.JobSucceeded {
		r.Job.Result = &core.Result{
			ID: id,
			Segments: []core.Segment{
				{Start: 0, End: 1, Text: "привет", Speaker: &a},
				{Start: 1, End: 2, Text: "здравствуйте", Speaker: &b},
			},
			Speakers: []core.Speaker{{ID: "spk_0", TotalSpeech: 1, Segments: 1}, {ID: "spk_1", TotalSpeech: 1, Segments: 1}},
		}
	}
	return r
}

func TestDiarizedSpeakersAreNamedAndKept(t *testing.T) {
	p := renameStack(t,
		diarized("job_a", "alice", core.JobSucceeded),
		diarized("job_q", "alice", core.JobQueued),
	)
	alice := core.WithCaller(context.Background(), core.Caller{KeyID: "alice"})
	bob := core.WithCaller(context.Background(), core.Caller{KeyID: "bob"})

	j, err := p.RenameSpeakers(alice, "job_a", map[string]string{"spk_0": "  Анна\n", "spk_1": "Борис"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Result.Speakers[0].Name != "Анна" || j.Result.Speakers[1].Name != "Борис" {
		t.Fatalf("answered with %+v", j.Result.Speakers)
	}
	// Kept: a later read has them, and the segments keep the ids.
	again, err := p.Job(alice, "job_a")
	if err != nil {
		t.Fatal(err)
	}
	if again.Result.Speakers[0].Name != "Анна" || *again.Result.Segments[0].Speaker != "spk_0" {
		t.Errorf("read back %+v", again.Result)
	}

	// One name at a time leaves the others; an empty one gives the id back.
	if j, _ = p.RenameSpeakers(alice, "job_a", map[string]string{"spk_1": ""}); j.Result.Speakers[0].Name != "Анна" ||
		j.Result.Speakers[1].Name != "" {
		t.Errorf("after clearing one: %+v", j.Result.Speakers)
	}

	// Nothing changes when any of it is wrong.
	for name, call := range map[string]func() error{
		"unknown speaker": func() error {
			_, err := p.RenameSpeakers(alice, "job_a", map[string]string{"spk_0": "Вера", "spk_9": "x"})
			return err
		},
		"too long": func() error {
			_, err := p.RenameSpeakers(alice, "job_a", map[string]string{"spk_0": strings.Repeat("я", core.MaxSpeakerName+1)})
			return err
		},
		"not finished": func() error {
			_, err := p.RenameSpeakers(alice, "job_q", map[string]string{"spk_0": "Вера"})
			return err
		},
	} {
		if err := call(); core.AsError(err).Code != core.CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if again, _ = p.Job(alice, "job_a"); again.Result.Speakers[0].Name != "Анна" {
		t.Errorf("half a rename applied: %+v", again.Result.Speakers)
	}

	// Someone else's job is not found, as everywhere.
	if _, err := p.RenameSpeakers(bob, "job_a", map[string]string{"spk_0": "x"}); core.AsError(err).Code != core.CodeJobNotFound {
		t.Errorf("another key: %v", err)
	}
}
