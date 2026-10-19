package native

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

type fakeIsolation struct {
	mu sync.Mutex

	iso     core.Isolation
	err     error
	formats []core.AudioFormat

	gotCreate     core.IsolationRequest
	gotUpload     []byte
	gotSelection  core.IsolationSelection
	gotFormat     core.AudioFormat
	gotSpeaker    string
	gotTranscribe core.Request
	gotUpdate     core.IsolationUpdate
	gotAll        bool
	gotSpeakers   core.Request
	transcript    *core.Result
	deleted       []string
}

func (f *fakeIsolation) List(context.Context) ([]core.Isolation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []core.Isolation{f.iso}, nil
}

func (f *fakeIsolation) Update(_ context.Context, _ string, upd core.IsolationUpdate) (*core.Isolation, error) {
	f.mu.Lock()
	f.gotUpdate = upd
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	v := f.iso
	return &v, nil
}

func (f *fakeIsolation) ExtractAll(context.Context, string) (*core.Isolation, error) {
	f.mu.Lock()
	f.gotAll = true
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	v := f.iso
	v.Status = core.IsolationQueued
	return &v, nil
}

func (f *fakeIsolation) Audio(_ context.Context, _ string, format core.AudioFormat) (io.ReadCloser, error) {
	f.mu.Lock()
	f.gotFormat = format
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return sized{strings.NewReader("RIFF-audio"), 10}, nil
}

func (f *fakeIsolation) TranscribeSpeakers(_ context.Context, _ string, req core.Request) (*core.Isolation, error) {
	f.mu.Lock()
	f.gotSpeakers = req
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	v := f.iso
	v.Status = core.IsolationQueued
	v.Transcript = &core.IsolationTranscript{Status: core.JobQueued, Model: req.ModelID}
	return &v, nil
}

func (f *fakeIsolation) Transcript(context.Context, string) (*core.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.transcript, nil
}

func (f *fakeIsolation) CanKeep() bool { return true }

func (f *fakeIsolation) Create(_ context.Context, req core.IsolationRequest) (*core.Isolation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotCreate = req
	// The handler deletes its spool when it returns: the service has to
	// have read the upload by then.
	rc, err := req.Audio.Open()
	if err != nil {
		return nil, err
	}
	f.gotUpload, _ = io.ReadAll(rc)
	_ = rc.Close()
	if f.err != nil {
		return nil, f.err
	}
	v := f.iso
	return &v, nil
}

func (f *fakeIsolation) Isolation(context.Context, string) (*core.Isolation, error) {
	if f.err != nil {
		return nil, f.err
	}
	v := f.iso
	return &v, nil
}

func (f *fakeIsolation) Watch(context.Context, string, int64) (<-chan core.IsolationEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan core.IsolationEvent, 2)
	a, b := f.iso, f.iso
	a.Status, b.Status = core.IsolationAnalysing, core.IsolationAwaitingSelection
	ch <- core.IsolationEvent{Seq: 3, Isolation: a}
	ch <- core.IsolationEvent{Seq: 4, Isolation: b}
	close(ch)
	return ch, nil
}

func (f *fakeIsolation) Sample(_ context.Context, _, speaker string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.gotSpeaker = speaker
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader("RIFF-sample")), nil
}

func (f *fakeIsolation) Extract(_ context.Context, _ string, sel core.IsolationSelection) (*core.Isolation, error) {
	f.mu.Lock()
	f.gotSelection = sel
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	v := f.iso
	v.Status = core.IsolationQueued
	return &v, nil
}

type sized struct {
	io.Reader
	n int64
}

func (s sized) Close() error { return nil }
func (s sized) Size() int64  { return s.n }

func (f *fakeIsolation) Result(_ context.Context, _, speaker string, format core.AudioFormat) (io.ReadCloser, error) {
	f.mu.Lock()
	f.gotFormat, f.gotSpeaker = format, speaker
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if format == core.AudioMP3 {
		return io.NopCloser(strings.NewReader("ID3-mp3")), nil
	}
	return sized{strings.NewReader("RIFF-result"), 11}, nil
}

func (f *fakeIsolation) Transcribe(_ context.Context, _, speaker string, req core.Request) (*core.Job, error) {
	f.mu.Lock()
	f.gotTranscribe, f.gotSpeaker = req, speaker
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &core.Job{ID: "job_7", Status: core.JobQueued}, nil
}

func (f *fakeIsolation) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, id)
	f.mu.Unlock()
	return f.err
}

func (f *fakeIsolation) Formats() []core.AudioFormat { return f.formats }

func isoServer(t *testing.T, iso core.IsolationService) (*fakeIsolation, string, *http.Client) {
	t.Helper()
	srv := newServer(t, &fakeService{}, adapter.Deps{Isolation: iso})
	f, _ := iso.(*fakeIsolation)
	return f, srv.URL, srv.Client()
}

func newFakeIsolation() *fakeIsolation {
	return &fakeIsolation{
		iso: core.Isolation{ID: "iso_1", Status: core.IsolationDone, Filename: "call.wav",
			Result: &core.IsolationResult{Speaker: "spk_1", Name: "call-spk_1", Duration: 10, SampleRate: 16000}},
		formats: []core.AudioFormat{core.AudioWAV, core.AudioMP3},
	}
}

func sendTo(t *testing.T, c *http.Client, method, url, contentType, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestIsolationInfoSaysWhetherItIsOn(t *testing.T) {
	srv := newServer(t, &fakeService{}, adapter.Deps{})
	var off map[string]any
	decode(t, do(t, srv, http.MethodGet, "/api/v1/isolation"), &off)
	if off["enabled"] != false {
		t.Errorf("without isolation: %v", off)
	}
	resp := upload(t, srv, "/api/v1/isolations")
	if p := problemOf(t, resp); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(p.Detail, "isolation.enabled") {
		t.Errorf("an upload without isolation: %d %+v", resp.StatusCode, p)
	}

	srv = newServer(t, &fakeService{}, adapter.Deps{Isolation: newFakeIsolation()})
	var on struct {
		Enabled bool     `json:"enabled"`
		Formats []string `json:"formats"`
		Keep    bool     `json:"keep"`
	}
	decode(t, do(t, srv, http.MethodGet, "/api/v1/isolation"), &on)
	if !on.Enabled || len(on.Formats) != 2 || !on.Keep {
		t.Errorf("with isolation: %+v", on)
	}
}

func TestCreateIsolation(t *testing.T) {
	iso := newFakeIsolation()
	srv := newServer(t, &fakeService{}, adapter.Deps{Isolation: iso})
	resp := upload(t, srv, "/api/v1/isolations", field{"num_speakers", "2"}, field{"keep", "true"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if iso.gotCreate.NumSpeakers != 2 || !iso.gotCreate.Keep || !bytes.HasPrefix(iso.gotUpload, []byte("RIFF")) {
		t.Errorf("service got %+v and %q", iso.gotCreate, iso.gotUpload)
	}
	resp = upload(t, srv, "/api/v1/isolations", field{"num_speakers", "two"})
	if p := problemOf(t, resp); p.Param != "num_speakers" {
		t.Errorf("a bad speaker count: %+v", p)
	}
}

func TestExtractTakesJSONOrAForm(t *testing.T) {
	iso := newFakeIsolation()
	_, url, c := isoServer(t, iso)

	resp := sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/extract", "application/json",
		`{"speaker":"spk_1","sample_start":12.5}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("json: %d", resp.StatusCode)
	}
	if iso.gotSelection.Speaker != "spk_1" || iso.gotSelection.SampleStart == nil || *iso.gotSelection.SampleStart != 12.5 {
		t.Errorf("json selection %+v", iso.gotSelection)
	}

	resp = sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/extract", "application/x-www-form-urlencoded",
		"speaker=spk_0")
	if resp.StatusCode != http.StatusAccepted || iso.gotSelection.Speaker != "spk_0" || iso.gotSelection.SampleStart != nil {
		t.Errorf("form: %d %+v", resp.StatusCode, iso.gotSelection)
	}

	for body, param := range map[string]string{
		`{"sample_start":1}`:                 "speaker",
		`{"speaker":"a","x":1}`:              "",
		`{"speaker":"a","sample_start":"x"}`: "",
	} {
		resp := sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/extract", "application/json", body)
		if p := problemOf(t, resp); resp.StatusCode != http.StatusBadRequest || p.Param != param {
			t.Errorf("%s: %d %+v", body, resp.StatusCode, p)
		}
	}
}

func TestResultIsServedAsAnAttachment(t *testing.T) {
	iso := newFakeIsolation()
	_, url, c := isoServer(t, iso)

	resp := sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/result", "", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "RIFF-result" {
		t.Fatalf("wav: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("Content-Type %q", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "11" {
		t.Errorf("Content-Length %q", cl)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename=call-spk_1.wav` {
		t.Errorf("Content-Disposition %q", cd)
	}

	resp = sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/result?format=MP3", "", "")
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "ID3-mp3" || resp.Header.Get("Content-Type") != "audio/mpeg" || iso.gotFormat != core.AudioMP3 {
		t.Errorf("mp3: %q %q", body, resp.Header.Get("Content-Type"))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "call-spk_1.mp3") {
		t.Errorf("mp3 Content-Disposition %q", cd)
	}
}

func TestSampleAndDelete(t *testing.T) {
	iso := newFakeIsolation()
	_, url, c := isoServer(t, iso)
	resp := sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/voices/spk_2/sample", "", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "RIFF-sample" || iso.gotSpeaker != "spk_2" {
		t.Errorf("sample: %d %q for %q", resp.StatusCode, body, iso.gotSpeaker)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		t.Errorf("a sample is played, not downloaded: %q", cd)
	}
	resp = sendTo(t, c, http.MethodDelete, url+"/api/v1/isolations/iso_1", "", "")
	if resp.StatusCode != http.StatusNoContent || len(iso.deleted) != 1 {
		t.Errorf("delete: %d %v", resp.StatusCode, iso.deleted)
	}
}

func TestTranscribeTakesTheJobParameters(t *testing.T) {
	iso := newFakeIsolation()
	_, url, c := isoServer(t, iso)
	resp := sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/transcribe",
		"application/x-www-form-urlencoded", "model=gigaam&language=ru&punctuate=true&hotwords=Ростелеком")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var job core.Job
	decode(t, resp, &job)
	got := iso.gotTranscribe
	if job.ID != "job_7" || got.ModelID != "gigaam" || got.Language != "ru" || !got.Punctuate ||
		len(got.Hotwords) != 1 || got.Audio != nil {
		t.Errorf("job %+v from %+v", job, got)
	}
	resp = sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/transcribe",
		"application/x-www-form-urlencoded", "punctuate=maybe")
	if p := problemOf(t, resp); p.Param != "punctuate" {
		t.Errorf("a bad parameter: %+v", p)
	}
}

func TestIsolationEventsStreamUntilIdle(t *testing.T) {
	iso := newFakeIsolation()
	_, url, c := isoServer(t, iso)
	resp := sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/events", "", "")
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{"id: 3\nevent: analysing", "id: 4\nevent: awaiting_selection", "event: done"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream lacks %q:\n%s", want, s)
		}
	}
}

func TestIsolationErrorsAreProblems(t *testing.T) {
	iso := newFakeIsolation()
	iso.err = core.Errorf(core.CodeIsolationNotFound, "no such isolation: iso_x")
	_, url, c := isoServer(t, iso)
	for _, path := range []string{"", "/events", "/result", "/voices/spk_0/sample"} {
		resp := sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_x"+path, "", "")
		if p := problemOf(t, resp); resp.StatusCode != http.StatusNotFound || p.Code != "isolation_not_found" {
			t.Errorf("%s: %d %+v", path, resp.StatusCode, p)
		}
	}
	iso.err = core.Errorf(core.CodeIsolationBusy, "busy")
	resp := sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/extract", "application/json", `{"speaker":"spk_0"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("busy: %d", resp.StatusCode)
	}
}

func TestTracksByVoice(t *testing.T) {
	iso := newFakeIsolation()
	iso.iso.Tracks = []core.IsolationResult{{Speaker: "spk_1", Name: "call-spk_1"}, {Speaker: "spk_0", Name: "call-Анна"}}
	_, url, c := isoServer(t, iso)

	resp := sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/tracks/spk_0?format=mp3", "", "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ID3-mp3" || iso.gotSpeaker != "spk_0" {
		t.Fatalf("track: %d %q for %q", resp.StatusCode, body, iso.gotSpeaker)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "call-%D0%90%D0%BD%D0%BD%D0%B0.mp3") {
		t.Errorf("Content-Disposition %q", cd)
	}
	sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/result?speaker=spk_1", "", "")
	if iso.gotSpeaker != "spk_1" {
		t.Errorf("result?speaker: %q", iso.gotSpeaker)
	}

	resp = sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/audio", "", "")
	body, _ = io.ReadAll(resp.Body)
	if string(body) != "RIFF-audio" || resp.Header.Get("Content-Disposition") != "attachment; filename=call.wav" {
		t.Errorf("audio: %q %q", body, resp.Header.Get("Content-Disposition"))
	}

	resp = sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/extract-all", "", "")
	if resp.StatusCode != http.StatusAccepted || !iso.gotAll {
		t.Errorf("extract-all: %d", resp.StatusCode)
	}
	resp = sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/transcribe",
		"application/x-www-form-urlencoded", "speaker=spk_0")
	if resp.StatusCode != http.StatusAccepted || iso.gotSpeaker != "spk_0" {
		t.Errorf("transcribe one track: %d %q", resp.StatusCode, iso.gotSpeaker)
	}
}

func TestUpdateNamesAndKeeps(t *testing.T) {
	iso := newFakeIsolation()
	_, url, c := isoServer(t, iso)
	resp := sendTo(t, c, http.MethodPatch, url+"/api/v1/isolations/iso_1", "application/json",
		`{"names":{"spk_0":"Анна"},"keep":true}`)
	if resp.StatusCode != http.StatusOK || iso.gotUpdate.Names["spk_0"] != "Анна" ||
		iso.gotUpdate.Keep == nil || !*iso.gotUpdate.Keep {
		t.Errorf("update: %d %+v", resp.StatusCode, iso.gotUpdate)
	}
	resp = sendTo(t, c, http.MethodPatch, url+"/api/v1/isolations/iso_1", "application/json", `{"name":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown field: %d", resp.StatusCode)
	}

	var list struct {
		Data []core.Isolation `json:"data"`
	}
	resp = sendTo(t, c, http.MethodGet, url+"/api/v1/isolations", "", "")
	decode(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].ID != "iso_1" {
		t.Errorf("list %+v", list)
	}
}

func TestTranscriptBySpeakers(t *testing.T) {
	iso := newFakeIsolation()
	a, b := "spk_0", "spk_1"
	iso.transcript = &core.Result{
		Text: "привет здравствуйте как дела",
		Segments: []core.Segment{
			{ID: 0, Start: 0, End: 1, Text: "привет", Speaker: &a},
			{ID: 1, Start: 1, End: 2, Text: "здравствуйте", Speaker: &b},
			{ID: 2, Start: 2, End: 3, Text: "как дела", Speaker: &b},
		},
		Speakers: []core.Speaker{{ID: "spk_0", Name: "Анна"}, {ID: "spk_1"}},
	}
	_, url, c := isoServer(t, iso)

	resp := sendTo(t, c, http.MethodPost, url+"/api/v1/isolations/iso_1/transcript",
		"application/x-www-form-urlencoded", "model=gigaam&language=ru&punctuate=true")
	var started core.Isolation
	decode(t, resp, &started)
	if resp.StatusCode != http.StatusAccepted || started.Transcript == nil || iso.gotSpeakers.ModelID != "gigaam" ||
		!iso.gotSpeakers.Punctuate {
		t.Fatalf("start: %d %+v", resp.StatusCode, iso.gotSpeakers)
	}

	var res core.Result
	decode(t, sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/transcript", "", ""), &res)
	// JSON keeps the ids in the segments and the names beside them.
	if *res.Segments[0].Speaker != "spk_0" || res.Speakers[0].Name != "Анна" {
		t.Errorf("json %+v", res)
	}
	resp = sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/transcript?response_format=text", "", "")
	body, _ := io.ReadAll(resp.Body)
	if want := "Анна: привет\n\nspk_1: здравствуйте как дела\n"; string(body) != want {
		t.Errorf("text %q, want %q", body, want)
	}
	resp = sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/transcript?response_format=vtt", "", "")
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<v Анна>привет</v>") || !strings.Contains(string(body), "<v spk_1>") {
		t.Errorf("vtt:\n%s", body)
	}
	if *iso.transcript.Segments[0].Speaker != "spk_0" {
		t.Error("rendering by name changed the transcript itself")
	}
	resp = sendTo(t, c, http.MethodGet, url+"/api/v1/isolations/iso_1/transcript?response_format=doc", "", "")
	if p := problemOf(t, resp); p.Param != "response_format" {
		t.Errorf("a bad format: %+v", p)
	}
}

func TestJobSpeakersAreNamed(t *testing.T) {
	a, b := "spk_0", "spk_1"
	svc := &fakeService{job: &core.Job{ID: "job_1", Status: core.JobSucceeded, Result: &core.Result{
		Segments: []core.Segment{
			{Start: 0, End: 1, Text: "привет", Speaker: &a},
			{Start: 1, End: 2, Text: "здравствуйте", Speaker: &b},
		},
		Speakers: []core.Speaker{{ID: "spk_0", Name: "Анна"}, {ID: "spk_1"}},
	}}}
	srv := newServer(t, svc, adapter.Deps{})
	c := srv.Client()

	resp := sendTo(t, c, http.MethodPatch, srv.URL+"/api/v1/jobs/job_1", "application/json",
		`{"speaker_names":{"spk_0":"Анна"}}`)
	if resp.StatusCode != http.StatusOK || svc.gotNames["spk_0"] != "Анна" {
		t.Fatalf("rename: %d %v", resp.StatusCode, svc.gotNames)
	}
	resp = sendTo(t, c, http.MethodPatch, srv.URL+"/api/v1/jobs/job_1", "application/json", `{"names":{}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown field: %d", resp.StatusCode)
	}

	// Subtitles label a cue by the name; an unnamed speaker keeps its id.
	resp = sendTo(t, c, http.MethodGet, srv.URL+"/api/v1/jobs/job_1?response_format=vtt", "", "")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<v Анна>привет</v>") || !strings.Contains(string(body), "<v spk_1>") {
		t.Errorf("vtt:\n%s", body)
	}
}
