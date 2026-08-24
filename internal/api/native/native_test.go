package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

// --- doubles ----------------------------------------------------------------

type fakeService struct {
	mu sync.Mutex

	result *core.Result
	job    *core.Job
	page   *core.JobPage
	events chan core.JobEvent

	err       error
	submitErr error
	watchErr  error

	gotRequest core.Request
	gotFilter  core.JobFilter
	canceled   []string
}

func (f *fakeService) Transcribe(_ context.Context, req core.Request) (*core.Result, error) {
	f.mu.Lock()
	f.gotRequest = req
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func (f *fakeService) Submit(_ context.Context, req core.Request) (*core.Job, error) {
	f.mu.Lock()
	f.gotRequest = req
	f.mu.Unlock()
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	return f.job, nil
}

func (f *fakeService) Job(context.Context, string) (*core.Job, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.job, nil
}

func (f *fakeService) ListJobs(_ context.Context, filter core.JobFilter) (*core.JobPage, error) {
	f.mu.Lock()
	f.gotFilter = filter
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.page, nil
}

func (f *fakeService) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	f.canceled = append(f.canceled, id)
	f.mu.Unlock()
	return f.err
}

func (f *fakeService) Watch(context.Context, string, int64) (<-chan core.JobEvent, error) {
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	return f.events, nil
}

type fakeModels struct {
	infos    []core.ModelInfo
	progress chan core.DownloadProgress
	err      error

	mu     sync.Mutex
	calls  []string
	pinned bool
}

func (f *fakeModels) List(context.Context) ([]core.ModelInfo, error)    { return f.infos, f.err }
func (f *fakeModels) Catalog(context.Context) ([]core.ModelInfo, error) { return f.infos, f.err }

func (f *fakeModels) Download(context.Context, string) (<-chan core.DownloadProgress, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.progress, nil
}

func (f *fakeModels) record(call string) error {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	return f.err
}

func (f *fakeModels) Load(_ context.Context, id string) error   { return f.record("load:" + id) }
func (f *fakeModels) Unload(_ context.Context, id string) error { return f.record("unload:" + id) }

func (f *fakeModels) Pin(_ context.Context, id string, pinned bool) error {
	f.mu.Lock()
	f.pinned = pinned
	f.mu.Unlock()
	return f.record("pin:" + id)
}

func (f *fakeModels) Reload(_ context.Context, id, rev string) error {
	return f.record("reload:" + id + "@" + rev)
}

// --- harness ----------------------------------------------------------------

func newServer(t *testing.T, svc core.Service, deps adapter.Deps) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Adapter{}).Mount(mux, svc, deps)

	// Admin by default: the caller is unauthenticated, which is open mode.
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// asKey runs a request as a specific, non-administrative caller.
func asKey(t *testing.T, svc core.Service, deps adapter.Deps, caller core.Caller) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Adapter{}).Mount(mux, svc, deps)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(core.WithCaller(r.Context(), caller)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type field struct{ key, value string }

func upload(t *testing.T, srv *httptest.Server, path string, fields ...field) *http.Response {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "call.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("RIFF....WAVEfake"))
	for _, f := range fields {
		if err := mw.WriteField(f.key, f.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+path, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func do(t *testing.T, srv *httptest.Server, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

func problemOf(t *testing.T, resp *http.Response) problem {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problem
	decode(t, resp, &p)
	return p
}

func sampleResult() *core.Result {
	return &core.Result{
		ID:              "txn_1",
		Model:           "gigaam-v2-ctc-ru",
		Language:        "ru",
		Duration:        2.5,
		Text:            "привет мир",
		TimestampSource: core.TimestampToken,
		Segments: []core.Segment{{
			ID: 0, Start: 0.5, End: 2.25, Text: "привет мир",
			Words: []core.Word{
				{Word: "привет", Start: 0.5, End: 1.2},
				{Word: "мир", Start: 1.4, End: 2.25},
			},
		}},
		Silence: []core.Silence{{Start: 0, End: 0.5}},
	}
}

// --- transcribe -------------------------------------------------------------

// The native dialect exists because OpenAI's json shape drops everything this
// server is for. Its default must be the whole result.
func TestTranscribeReturnsTheWholeResult(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/transcribe")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got core.Result
	decode(t, resp, &got)

	if got.Text != "привет мир" || got.TimestampSource != core.TimestampToken {
		t.Errorf("result = %+v", got)
	}
	if len(got.Segments) != 1 || len(got.Segments[0].Words) != 2 {
		t.Fatalf("got %d segments", len(got.Segments))
	}
	if len(got.Silence) != 1 {
		t.Errorf("silence was dropped: %+v", got.Silence)
	}
}

func TestTranscribePassesEveryParameterThrough(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/transcribe",
		field{"model", "gigaam-v2-rnnt-ru"},
		field{"language", "ru"},
		field{"channel_mode", "split"},
		field{"decoding_method", "modified_beam_search"},
		field{"max_active_paths", "8"},
		field{"diarize", "true"},
		field{"num_speakers", "2"},
		field{"punctuate", "yes"},
		field{"itn", "1"},
		field{"hotwords", "нанoasr, телефония"},
		field{"hotwords[]", "распознавание"},
		field{"hotwords_score", "1.5"},
		field{"webhook_url", "https://example.test/hook"},
	)

	got := svc.gotRequest
	if got.ModelID != "gigaam-v2-rnnt-ru" || got.Language != "ru" {
		t.Errorf("model/language = %q/%q", got.ModelID, got.Language)
	}
	if got.ChannelMode != core.ChannelSplit || got.DecodingMethod != "modified_beam_search" {
		t.Errorf("channel/decoding = %q/%q", got.ChannelMode, got.DecodingMethod)
	}
	if got.MaxActivePaths != 8 || got.NumSpeakers != 2 {
		t.Errorf("paths/speakers = %d/%d", got.MaxActivePaths, got.NumSpeakers)
	}
	if !got.Diarize || !got.Punctuate || !got.ITN {
		t.Errorf("flags = %v/%v/%v", got.Diarize, got.Punctuate, got.ITN)
	}
	if len(got.Hotwords) != 3 {
		t.Errorf("hotwords = %v, want the comma-separated and repeated forms merged", got.Hotwords)
	}
	if got.HotwordsScore != 1.5 || got.WebhookURL != "https://example.test/hook" {
		t.Errorf("score/webhook = %v/%q", got.HotwordsScore, got.WebhookURL)
	}
	if got.Source != core.SourceAPI {
		t.Errorf("source = %q", got.Source)
	}
}

// A mistyped value must be named, not ignored: a caller who wrote punctate=true
// and got no punctuation would have no way to find out why.
func TestTranscribeRejectsBadParametersByName(t *testing.T) {
	cases := []struct {
		field field
		param string
	}{
		{field{"response_format", "yaml"}, "response_format"},
		{field{"channel_mode", "stereo"}, "channel_mode"},
		{field{"decoding_method", "beam"}, "decoding_method"},
		{field{"diarize", "maybe"}, "diarize"},
		{field{"num_speakers", "-1"}, "num_speakers"},
		{field{"hotwords_score", "loud"}, "hotwords_score"},
	}
	for _, c := range cases {
		t.Run(c.param, func(t *testing.T) {
			svc := &fakeService{result: sampleResult()}
			resp := upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/transcribe", c.field)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			p := problemOf(t, resp)
			if p.Code != string(core.CodeInvalidRequest) || p.Param != c.param {
				t.Errorf("problem = %+v", p)
			}
		})
	}
}

func TestTranscribeRendersEveryFormat(t *testing.T) {
	cases := []struct {
		format      string
		contentType string
		contains    string
	}{
		{"text", "text/plain", "привет мир"},
		{"srt", "application/x-subrip", "00:00:00,500 --> 00:00:02,250"},
		{"vtt", "text/vtt", "WEBVTT"},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			svc := &fakeService{result: sampleResult()}
			resp := upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/transcribe",
				field{"response_format", c.format})

			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.contentType) {
				t.Errorf("Content-Type = %q, want %q", ct, c.contentType)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), c.contains) {
				t.Errorf("body = %q, want it to contain %q", body, c.contains)
			}
		})
	}
}

func TestTranscribeReportsAMissingFile(t *testing.T) {
	srv := newServer(t, &fakeService{}, adapter.Deps{})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "gigaam-v2-ctc-ru")
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/transcribe", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if p := problemOf(t, resp); p.Param != "file" {
		t.Errorf("problem = %+v, want it to name the file part", p)
	}
}

func TestDomainErrorsBecomeProblemDocuments(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{core.Errorf(core.CodeModelNotFound, "no such model"), http.StatusNotFound},
		{core.Errorf(core.CodeUnsupportedMediaType, "not audio"), http.StatusUnsupportedMediaType},
		{core.Errorf(core.CodeCapabilityUnavailable, "no words"), http.StatusUnprocessableEntity},
		{core.Errorf(core.CodeProcessingTimeout, "too slow"), http.StatusGatewayTimeout},
		{core.Errorf(core.CodeModelUnavailable, "no slot"), http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(string(core.AsError(c.err).Code), func(t *testing.T) {
			svc := &fakeService{err: c.err}
			resp := upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/transcribe")

			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
			p := problemOf(t, resp)
			if p.Status != c.status || p.Code != string(core.AsError(c.err).Code) {
				t.Errorf("problem = %+v", p)
			}
			if p.Instance != "/api/v1/transcribe" {
				t.Errorf("instance = %q", p.Instance)
			}
		})
	}
}

// Without Retry-After a client told to back off has no idea how far, and in
// practice retries immediately — which is what caused the 429.
func TestQueueFullAdvisesWhenToRetry(t *testing.T) {
	svc := &fakeService{submitErr: core.Errorf(core.CodeQueueFull, "full")}
	resp := upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/jobs")

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
}

// --- jobs -------------------------------------------------------------------

func TestSubmitAnswers202WithTheQueuedJob(t *testing.T) {
	svc := &fakeService{job: &core.Job{
		ID: "job_1", Status: core.JobQueued, Position: 3, CreatedAt: time.Now(),
	}}
	resp := upload(t, newServer(t, svc, adapter.Deps{}), "/api/v1/jobs",
		field{"model", "gigaam-v2-ctc-ru"})

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var got core.Job
	decode(t, resp, &got)
	if got.ID != "job_1" || got.Status != core.JobQueued || got.Position != 3 {
		t.Errorf("job = %+v", got)
	}
}

func TestListJobsRendersAPageWithItsCursor(t *testing.T) {
	svc := &fakeService{page: &core.JobPage{
		Jobs:       []core.Job{{ID: "job_1", Status: core.JobSucceeded}},
		NextCursor: "abc",
	}}
	resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet, "/api/v1/jobs")

	var got core.JobPage
	decode(t, resp, &got)
	if len(got.Jobs) != 1 || got.NextCursor != "abc" {
		t.Errorf("page = %+v", got)
	}
}

func TestListJobsParsesItsFilters(t *testing.T) {
	svc := &fakeService{page: &core.JobPage{}}
	do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet,
		"/api/v1/jobs?status=failed&status=canceled&model=gigaam-v2-ctc-ru"+
			"&source=ui&limit=10&cursor=abc&since=2026-01-02T15%3A04%3A05Z")

	f := svc.gotFilter
	if len(f.Status) != 2 || f.Status[0] != core.JobFailed {
		t.Errorf("status = %v", f.Status)
	}
	if f.ModelID != "gigaam-v2-ctc-ru" || f.Source != core.SourceUI {
		t.Errorf("model/source = %q/%q", f.ModelID, f.Source)
	}
	if f.Limit != 10 || f.Cursor != "abc" || f.Since == nil {
		t.Errorf("limit/cursor/since = %d/%q/%v", f.Limit, f.Cursor, f.Since)
	}
}

func TestListJobsRejectsBadFilters(t *testing.T) {
	for _, q := range []string{"?status=nonsense", "?source=cron", "?limit=0", "?since=yesterday"} {
		t.Run(q, func(t *testing.T) {
			svc := &fakeService{page: &core.JobPage{}}
			resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet, "/api/v1/jobs"+q)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// The service decides ownership; a 404 is what reaches the client, because a
// 403 would confirm the job exists under someone else's key.
func TestAnotherKeysJobIsNotFound(t *testing.T) {
	svc := &fakeService{err: core.Errorf(core.CodeJobNotFound, "no such job: job_1")}
	resp := do(t, asKey(t, svc, adapter.Deps{}, core.Caller{KeyID: "key-b"}),
		http.MethodGet, "/api/v1/jobs/job_1")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if p := problemOf(t, resp); p.Code != string(core.CodeJobNotFound) {
		t.Errorf("problem = %+v", p)
	}
}

func TestAFinishedJobCanBeFetchedAsSubtitles(t *testing.T) {
	svc := &fakeService{job: &core.Job{
		ID: "job_1", Status: core.JobSucceeded, Result: sampleResult(),
	}}
	resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet,
		"/api/v1/jobs/job_1?response_format=srt")

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-subrip") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "привет мир") {
		t.Errorf("body = %q", body)
	}
}

func TestAnUnfinishedJobCannotBeRenderedAsSubtitles(t *testing.T) {
	svc := &fakeService{job: &core.Job{ID: "job_1", Status: core.JobRunning}}
	resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet,
		"/api/v1/jobs/job_1?response_format=vtt")

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if p := problemOf(t, resp); p.Param != "response_format" {
		t.Errorf("problem = %+v", p)
	}
}

// Reporting "canceled" for a job that had already succeeded would be a lie the
// client then acts on, so the handler reads the state back.
func TestCancelReportsTheStateThatActuallyResulted(t *testing.T) {
	svc := &fakeService{job: &core.Job{ID: "job_1", Status: core.JobSucceeded}}
	resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodDelete, "/api/v1/jobs/job_1")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got core.Job
	decode(t, resp, &got)
	if got.Status != core.JobSucceeded {
		t.Errorf("status = %q, want the state the service actually settled on", got.Status)
	}
	if len(svc.canceled) != 1 || svc.canceled[0] != "job_1" {
		t.Errorf("cancelled %v", svc.canceled)
	}
}

// --- SSE --------------------------------------------------------------------

// readEvents pulls SSE frames until the stream ends.
func readEvents(t *testing.T, body io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestJobEventsStreamsTransitions(t *testing.T) {
	events := make(chan core.JobEvent, 3)
	events <- core.JobEvent{Seq: 1, Job: core.Job{ID: "job_1", Status: core.JobQueued}}
	events <- core.JobEvent{Seq: 2, Job: core.Job{ID: "job_1", Status: core.JobRunning, Stage: "asr", Percent: 40}}
	events <- core.JobEvent{Seq: 3, Job: core.Job{ID: "job_1", Status: core.JobSucceeded}}
	close(events)

	svc := &fakeService{events: events}
	resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet, "/api/v1/jobs/job_1/events")

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	lines := readEvents(t, resp.Body)
	joined := strings.Join(lines, "\n")

	for _, want := range []string{
		"id: 1", "event: queued",
		"id: 2", "event: running",
		"id: 3", "event: succeeded",
		// An EventSource reconnects when the server closes a stream, so the
		// end of the work has to be stated rather than implied.
		"event: done",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream is missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, `"stage":"asr"`) {
		t.Errorf("stage was not carried:\n%s", joined)
	}
}

// A 404 has to be a 404. Once the stream headers are out, the only place left
// to report a refusal is inside a 200.
func TestJobEventsRefusesBeforeStreaming(t *testing.T) {
	svc := &fakeService{watchErr: core.Errorf(core.CodeJobNotFound, "no such job")}
	resp := do(t, newServer(t, svc, adapter.Deps{}), http.MethodGet, "/api/v1/jobs/nope/events")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if p := problemOf(t, resp); p.Code != string(core.CodeJobNotFound) {
		t.Errorf("problem = %+v", p)
	}
}

func TestDownloadStreamsProgress(t *testing.T) {
	progress := make(chan core.DownloadProgress, 2)
	progress <- core.DownloadProgress{ModelID: "m", Downloaded: 50, Total: 100, Percent: 50}
	progress <- core.DownloadProgress{ModelID: "m", Downloaded: 100, Total: 100, Percent: 100, Done: true}
	close(progress)

	models := &fakeModels{progress: progress}
	resp := do(t, newServer(t, &fakeService{}, adapter.Deps{Models: models}),
		http.MethodPost, "/api/v1/models/m/download")

	joined := strings.Join(readEvents(t, resp.Body), "\n")
	if !strings.Contains(joined, "event: progress") || !strings.Contains(joined, "event: done") {
		t.Errorf("stream = %s", joined)
	}
	if !strings.Contains(joined, `"percent":50`) {
		t.Errorf("progress was not carried: %s", joined)
	}
}

func TestDownloadRefusesBeforeStreaming(t *testing.T) {
	models := &fakeModels{err: core.Errorf(core.CodeModelForbidden, "non-commercial licence")}
	resp := do(t, newServer(t, &fakeService{}, adapter.Deps{Models: models}),
		http.MethodPost, "/api/v1/models/m/download")

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// --- models -----------------------------------------------------------------

func TestModelsAndCatalogAreReadableByAnyKey(t *testing.T) {
	models := &fakeModels{infos: []core.ModelInfo{{ID: "gigaam-v2-ctc-ru", State: core.ModelReady}}}
	srv := asKey(t, &fakeService{}, adapter.Deps{Models: models}, core.Caller{KeyID: "key-a"})

	for _, path := range []string{"/api/v1/models", "/api/v1/catalog"} {
		resp := do(t, srv, http.MethodGet, path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		var got struct {
			Data []core.ModelInfo `json:"data"`
		}
		decode(t, resp, &got)
		if len(got.Data) != 1 || got.Data[0].ID != "gigaam-v2-ctc-ru" {
			t.Errorf("%s: data = %+v", path, got.Data)
		}
	}
}

func TestChangingModelStateRequiresAnAdminKey(t *testing.T) {
	models := &fakeModels{}
	srv := asKey(t, &fakeService{}, adapter.Deps{Models: models}, core.Caller{KeyID: "key-a"})

	for _, path := range []string{
		"/api/v1/models/m/load", "/api/v1/models/m/unload",
		"/api/v1/models/m/pin", "/api/v1/models/m/reload", "/api/v1/models/m/download",
	} {
		resp := do(t, srv, http.MethodPost, path)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, resp.StatusCode)
		}
		// The refusal has to speak this dialect, or a client's error handling
		// does not recognise it.
		if p := problemOf(t, resp); p.Code != string(core.CodeModelForbidden) {
			t.Errorf("%s: problem = %+v", path, p)
		}
	}
	if len(models.calls) != 0 {
		t.Errorf("a non-admin key reached the model service: %v", models.calls)
	}
}

func TestConfigIsAdminOnly(t *testing.T) {
	deps := adapter.Deps{ConfigSnapshot: func() any { return map[string]string{"addr": ":8080"} }}

	denied := do(t, asKey(t, &fakeService{}, deps, core.Caller{KeyID: "key-a"}),
		http.MethodGet, "/api/v1/config")
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", denied.StatusCode)
	}

	allowed := do(t, newServer(t, &fakeService{}, deps), http.MethodGet, "/api/v1/config")
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d", allowed.StatusCode)
	}
	var got map[string]string
	decode(t, allowed, &got)
	if got["addr"] != ":8080" {
		t.Errorf("config = %v", got)
	}
}

func TestModelActionsReachTheServiceAndReportTheNewState(t *testing.T) {
	models := &fakeModels{infos: []core.ModelInfo{{ID: "m", State: core.ModelReady, Pinned: true}}}
	srv := newServer(t, &fakeService{}, adapter.Deps{Models: models})

	resp := do(t, srv, http.MethodPost, "/api/v1/models/m/load")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var info core.ModelInfo
	decode(t, resp, &info)
	if info.State != core.ModelReady {
		t.Errorf("state = %q, want the state after the action", info.State)
	}

	do(t, srv, http.MethodPost, "/api/v1/models/m/unload")
	do(t, srv, http.MethodPost, "/api/v1/models/m/reload?revision=v3")

	want := []string{"load:m", "unload:m", "reload:m@v3"}
	if strings.Join(models.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", models.calls, want)
	}
}

// A bare POST pins; the same endpoint unpins with pinned=false, so a UI toggle
// does not need two routes.
func TestPinDefaultsToPinningAndCanUnpin(t *testing.T) {
	models := &fakeModels{infos: []core.ModelInfo{{ID: "m"}}}
	srv := newServer(t, &fakeService{}, adapter.Deps{Models: models})

	do(t, srv, http.MethodPost, "/api/v1/models/m/pin")
	if !models.pinned {
		t.Error("a bare pin did not pin")
	}
	do(t, srv, http.MethodPost, "/api/v1/models/m/pin?pinned=false")
	if models.pinned {
		t.Error("pinned=false did not unpin")
	}
}

func TestModelEndpointsReportAMissingModelService(t *testing.T) {
	srv := newServer(t, &fakeService{}, adapter.Deps{})
	resp := do(t, srv, http.MethodGet, "/api/v1/models")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}
