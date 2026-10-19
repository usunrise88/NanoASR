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
	deleted       []string
}

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

func (f *fakeIsolation) Result(_ context.Context, _ string, format core.AudioFormat) (io.ReadCloser, error) {
	f.mu.Lock()
	f.gotFormat = format
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if format == core.AudioMP3 {
		return io.NopCloser(strings.NewReader("ID3-mp3")), nil
	}
	return sized{strings.NewReader("RIFF-result"), 11}, nil
}

func (f *fakeIsolation) Transcribe(_ context.Context, _ string, req core.Request) (*core.Job, error) {
	f.mu.Lock()
	f.gotTranscribe = req
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
	}
	decode(t, do(t, srv, http.MethodGet, "/api/v1/isolation"), &on)
	if !on.Enabled || len(on.Formats) != 2 {
		t.Errorf("with isolation: %+v", on)
	}
}

func TestCreateIsolation(t *testing.T) {
	iso := newFakeIsolation()
	srv := newServer(t, &fakeService{}, adapter.Deps{Isolation: iso})
	resp := upload(t, srv, "/api/v1/isolations", field{"num_speakers", "2"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if iso.gotCreate.NumSpeakers != 2 || !bytes.HasPrefix(iso.gotUpload, []byte("RIFF")) {
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
