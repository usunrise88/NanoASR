package era

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

// --- fakes ------------------------------------------------------------------

type fakeService struct {
	result *core.Result
	err    error
	got    core.Request

	job       *core.Job
	submitted core.Request
	submitErr error
	jobErr    error
}

func (f *fakeService) Transcribe(_ context.Context, req core.Request) (*core.Result, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	res := *f.result
	res.Warnings = append([]core.Warning(nil), f.result.Warnings...)
	return &res, nil
}

func (f *fakeService) Submit(_ context.Context, req core.Request) (*core.Job, error) {
	f.submitted = req
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	return f.job, nil
}

func (f *fakeService) Job(context.Context, string) (*core.Job, error) {
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	return f.job, nil
}

func (f *fakeService) ListJobs(context.Context, core.JobFilter) (*core.JobPage, error) {
	return nil, core.ErrNotImplemented
}
func (f *fakeService) Cancel(context.Context, string) error { return core.ErrNotImplemented }
func (f *fakeService) Watch(context.Context, string, int64) (<-chan core.JobEvent, error) {
	return nil, core.ErrNotImplemented
}

type fakeModels struct{ infos []core.ModelInfo }

func (f fakeModels) List(context.Context) ([]core.ModelInfo, error)    { return f.infos, nil }
func (f fakeModels) Catalog(context.Context) ([]core.ModelInfo, error) { return f.infos, nil }
func (fakeModels) Download(context.Context, string) (<-chan core.DownloadProgress, error) {
	return nil, core.ErrNotImplemented
}
func (fakeModels) Load(context.Context, string) error           { return nil }
func (fakeModels) Unload(context.Context, string) error         { return nil }
func (fakeModels) Pin(context.Context, string, bool) error      { return nil }
func (fakeModels) Reload(context.Context, string, string) error { return nil }

func sampleResult() *core.Result {
	speaker := "spk_0"
	return &core.Result{
		ID: "txn_1", Model: "gigaam-v3-ctc-punct-ru@2025-12-16", Language: "ru", Duration: 3.5,
		Text:            "Привет, мир",
		TimestampSource: core.TimestampToken,
		Segments: []core.Segment{{
			ID: 0, Start: 0.5, End: 2.04, Text: "Привет, мир", Speaker: &speaker,
			AvgConfidence: 0.81,
			Words: []core.Word{
				{Word: "Привет,", Start: 0.5, End: 1.1, Confidence: 0.9},
				{Word: "мир", Start: 1.2, End: 2.04, Confidence: 0.72},
			},
		}},
		Stats: core.Stats{AudioDuration: 3.5, RTF: 0.12, ProcessingMS: 420},
	}
}

// --- harness ----------------------------------------------------------------

func newServer(t *testing.T, svc core.Service, models core.ModelService) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Adapter{}).Mount(mux, svc, adapter.Deps{Models: models, MaxUploadBytes: 10 << 20})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// post uploads under the field name this contract uses, with the parameters in
// the query string where the contract puts them.
func post(t *testing.T, srv *httptest.Server, path, filename string, query url.Values) *http.Response {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(uploadField, filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("RIFF....WAVEfake"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	target := srv.URL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodPost, target, &body)
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

func get(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

func query(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

// --- /asr -------------------------------------------------------------------

// The default output is txt served as text/plain with the two headers this
// contract promises. All three are what a borrowed client keys off.
func TestASRDefaultsToPlainText(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, bodyOf(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if engine := resp.Header.Get("Asr-Engine"); engine != engineName {
		t.Errorf("Asr-Engine = %q, want %q", engine, engineName)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="call.wav.txt"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if got := bodyOf(t, resp); got != "Привет, мир\n" {
		t.Errorf("body = %q", got)
	}
}

// Every output format is served as text/plain, json included: a client of this
// contract saves the body under the filename it was given, and branching the
// content type on one of the five would break that.
func TestASROutputFormats(t *testing.T) {
	for _, tc := range []struct {
		output   string
		contains string
	}{
		{outputTXT, "Привет, мир"},
		{outputSRT, "00:00:00,500 --> 00:00:02,040"},
		{outputVTT, "WEBVTT"},
		{outputTSV, "start\tend\ttext\n500\t2040\tПривет, мир\n"},
		{outputJSON, `"language": "ru"`},
	} {
		t.Run(tc.output, func(t *testing.T) {
			svc := &fakeService{result: sampleResult()}
			resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav",
				query("output", tc.output))

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain for every output", ct)
			}
			want := `attachment; filename="call.wav.` + tc.output + `"`
			if cd := resp.Header.Get("Content-Disposition"); cd != want {
				t.Errorf("Content-Disposition = %q, want %q", cd, want)
			}
			if got := bodyOf(t, resp); !strings.Contains(got, tc.contains) {
				t.Errorf("body does not contain %q:\n%s", tc.contains, got)
			}
		})
	}
}

// word_timestamps chooses what the json output carries. The pipeline produces
// words either way, so the flag has to actually remove them.
func TestASRJSONWordsFollowWordTimestamps(t *testing.T) {
	for _, tc := range []struct {
		flag      string
		wantWords bool
	}{{"", false}, {"false", false}, {"true", true}} {
		svc := &fakeService{result: sampleResult()}
		q := url.Values{"output": {outputJSON}}
		if tc.flag != "" {
			q.Set("word_timestamps", tc.flag)
		}
		resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", q)

		got := strings.Contains(bodyOf(t, resp), `"word": "Привет,"`)
		if got != tc.wantWords {
			t.Errorf("word_timestamps=%q: words present = %v, want %v", tc.flag, got, tc.wantWords)
		}
	}
}

// A rendered result must not lose its words for the caller that held it: the
// json writer strips them from a copy, not from the result it was handed.
func TestJSONWithoutWordsDoesNotMutateTheResult(t *testing.T) {
	res := sampleResult()
	if _, err := transcriptJSON(res, false); err != nil {
		t.Fatal(err)
	}
	if len(res.Segments[0].Words) != 2 {
		t.Errorf("rendering stripped words from the caller's result")
	}
}

func TestASRRejectsAnUnknownOutput(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", query("output", "docx"))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env errorEnvelope
	decodeBody(t, resp, &env)
	if !strings.Contains(env.Detail.Message, "docx") || env.Detail.Param != "output" {
		t.Errorf("detail = %+v", env.Detail)
	}
}

// Answering a translation request with a transcription is the one degradation
// a caller cannot spot in the body, so it is refused instead.
func TestASRRefusesTranslation(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", query("task", "translate"))

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// --- parameter mapping ------------------------------------------------------

func TestASRMapsParametersOntoTheRequest(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", query(
		"language", "ru",
		"diarize", "true",
		"min_speakers", "2",
		"max_speakers", "2",
		"initial_prompt", "Ромашка, ИНН",
	))

	got := svc.got
	if got.Language != "ru" || !got.Diarize {
		t.Errorf("request = %+v", got)
	}
	if got.NumSpeakers != 2 {
		t.Errorf("NumSpeakers = %d, want 2 when min equals max", got.NumSpeakers)
	}
	if len(got.Hotwords) != 2 || got.Hotwords[0] != "Ромашка" || got.Hotwords[1] != "ИНН" {
		t.Errorf("Hotwords = %v, want the prompt split on commas", got.Hotwords)
	}
}

// A range has nothing for the clusterer to cut at, so it is dropped — and said
// so, because dropping it silently is how a caller ends up believing they
// constrained something.
func TestASRSpeakerRangeIsReducedWithAWarning(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", query(
		"diarize", "true", "min_speakers", "1", "max_speakers", "4"))

	if svc.got.NumSpeakers != 0 {
		t.Errorf("NumSpeakers = %d, want 0: a range is not a count", svc.got.NumSpeakers)
	}
	if w := resp.Header.Get(warningsHeader); !strings.Contains(w, "speaker_range_reduced") {
		t.Errorf("%s = %q, want speaker_range_reduced", warningsHeader, w)
	}
}

// Inert parameters warn when the caller sent them and stay quiet when they did
// not: a warning on every default would teach clients to ignore the header.
func TestInertParametersWarnOnlyWhenSent(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", nil)
	if w := resp.Header.Get(warningsHeader); w != "" {
		t.Errorf("%s = %q, want empty for a request with no options", warningsHeader, w)
	}

	svc = &fakeService{result: sampleResult()}
	resp = post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav",
		query("encode", "false", "vad_filter", "true"))
	w := resp.Header.Get(warningsHeader)
	if !strings.Contains(w, "encode_ignored") || !strings.Contains(w, "vad_filter_ignored") {
		t.Errorf("%s = %q, want both inert parameters named", warningsHeader, w)
	}
}

func TestASRRejectsAMalformedBoolean(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr", "call.wav", query("diarize", "maybe"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// The filename travels inside a quoted header value, so a name carrying a quote
// or a newline must not be able to end it.
func TestContentDispositionEscapesTheFilename(t *testing.T) {
	got := contentDisposition(`ев"ро\nотчёт.wav`, outputSRT)
	if strings.Count(got, `"`) != 2 {
		t.Errorf("Content-Disposition has an unbalanced quote: %q", got)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(got, `attachment; filename="`), `"`)
	for _, bad := range []string{"\n", "\r", `"`} {
		if strings.Contains(inner, bad) {
			t.Errorf("Content-Disposition carries a raw %q: %q", bad, got)
		}
	}
	if !strings.HasSuffix(got, `.srt"`) {
		t.Errorf("the extension must be inside the quotes: %q", got)
	}
}

// A Cyrillic filename must come back single-encoded: %D0%B7, not %C3%90%C2%B7.
func TestEscapeFilenameDoesNotDoubleEncode(t *testing.T) {
	if got, want := escapeFilename("звонок.wav"), "%D0%B7%D0%B2%D0%BE%D0%BD%D0%BE%D0%BA.wav"; got != want {
		t.Errorf("escapeFilename = %q, want %q", got, want)
	}
	if got := escapeFilename("call-1_v2.wav"); got != "call-1_v2.wav" {
		t.Errorf("an ascii filename must pass through untouched, got %q", got)
	}
}

// --- /detect-language -------------------------------------------------------

func TestDetectLanguage(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	models := fakeModels{infos: []core.ModelInfo{{
		ID: "gigaam-v3-ctc-punct-ru", Revision: "2025-12-16", Languages: []string{"ru"},
	}}}
	resp := post(t, newServer(t, svc, models), "/detect-language", "call.wav", nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got languageResponse
	decodeBody(t, resp, &got)
	if got.LanguageCode != "ru" || got.DetectedLanguage != "russian" || got.Confidence != 1 {
		t.Errorf("response = %+v", got)
	}
}

// A model that lists several languages was not detected, it was picked. The
// confidence says so rather than rounding up to certainty.
func TestDetectLanguageIsHonestAboutAMultilingualModel(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	models := fakeModels{infos: []core.ModelInfo{{
		ID: "gigaam-v3-ctc-punct-ru", Revision: "2025-12-16", Languages: []string{"ru", "en"},
	}}}
	resp := post(t, newServer(t, svc, models), "/detect-language", "call.wav", nil)

	var got languageResponse
	decodeBody(t, resp, &got)
	if got.Confidence != 0 {
		t.Errorf("confidence = %v, want 0: nothing measured this", got.Confidence)
	}
	if w := resp.Header.Get(warningsHeader); !strings.Contains(w, "language_detection_unavailable") {
		t.Errorf("%s = %q", warningsHeader, w)
	}
}

// --- /asr_task --------------------------------------------------------------

func queuedJob() *core.Job {
	return &core.Job{
		ID: "job_abc123", Status: core.JobQueued, Filename: "call.wav",
		ModelID: "gigaam-v3-ctc-punct-ru", CreatedAt: time.Unix(0, 0).UTC(),
	}
}

func TestAsrTaskReturnsAHandle(t *testing.T) {
	svc := &fakeService{result: sampleResult(), job: queuedJob()}
	resp := post(t, newServer(t, svc, fakeModels{}), "/asr_task", "call.wav",
		query("output", outputSRT, "diarize", "true"))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got taskAccepted
	decodeBody(t, resp, &got)
	if !got.Success {
		t.Error("success = false")
	}
	if got.TaskID != "job_abc123"+taskIDSeparator+outputSRT {
		t.Errorf("task_id = %q", got.TaskID)
	}
	if !svc.submitted.Diarize {
		t.Error("the submitted request lost diarize=true")
	}
}

// A task still running answers success true and is_ready false — success turns
// false only when the task finished and failed.
func TestAsrTaskByIDWhileRunning(t *testing.T) {
	job := queuedJob()
	job.Status = core.JobRunning
	svc := &fakeService{result: sampleResult(), job: job}

	resp := get(t, newServer(t, svc, fakeModels{}), "/asr_task/job_abc123~txt")
	var got taskState
	decodeBody(t, resp, &got)
	if !got.Success || got.IsReady || got.Error != nil {
		t.Errorf("state = %+v, want success true and is_ready false", got)
	}
}

// A finished task answers with the transcript itself, in the format the
// submission asked for, carried in the task id.
func TestAsrTaskByIDReturnsTheTranscriptInTheSubmittedFormat(t *testing.T) {
	job := queuedJob()
	job.Status = core.JobSucceeded
	job.Result = sampleResult()
	svc := &fakeService{result: sampleResult(), job: job}

	resp := get(t, newServer(t, svc, fakeModels{}), "/asr_task/job_abc123~srt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="call.wav.srt"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if body := bodyOf(t, resp); !strings.Contains(body, "-->") {
		t.Errorf("body is not SRT:\n%s", body)
	}
}

// A bare job id still works, and ?output= still wins, so a client that stored
// only the id is not stuck with the default.
func TestAsrTaskByIDAcceptsABareIDAndAnExplicitOutput(t *testing.T) {
	job := queuedJob()
	job.Status = core.JobSucceeded
	job.Result = sampleResult()
	svc := &fakeService{result: sampleResult(), job: job}
	srv := newServer(t, svc, fakeModels{})

	if cd := get(t, srv, "/asr_task/job_abc123").Header.Get("Content-Disposition"); cd !=
		`attachment; filename="call.wav.txt"` {
		t.Errorf("a bare id should fall back to txt, got %q", cd)
	}
	if cd := get(t, srv, "/asr_task/job_abc123~txt?output=vtt").Header.Get("Content-Disposition"); cd !=
		`attachment; filename="call.wav.vtt"` {
		t.Errorf("an explicit output should win, got %q", cd)
	}
}

func TestAsrTaskByIDReportsAFailure(t *testing.T) {
	job := queuedJob()
	job.Status = core.JobFailed
	job.Error = &core.Error{Code: core.CodeUnsupportedMediaType, Message: "unsupported audio format"}
	svc := &fakeService{result: sampleResult(), job: job}

	resp := get(t, newServer(t, svc, fakeModels{}), "/asr_task/job_abc123~txt")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a reported failure is not a transport failure", resp.StatusCode)
	}
	var got taskState
	decodeBody(t, resp, &got)
	if got.Success || !got.IsReady || got.Error == nil {
		t.Fatalf("state = %+v", got)
	}
	if got.Error.Title != string(core.CodeUnsupportedMediaType) {
		t.Errorf("title = %q", got.Error.Title)
	}
	if len(got.Error.Traceback) == 0 {
		t.Error("traceback is empty; the field is part of the contract")
	}
}

// Cancelled and expired are terminal without an error of their own, and
// "ready, failed, no reason" would be worse than naming the state.
func TestAsrTaskByIDReportsACancelledTask(t *testing.T) {
	job := queuedJob()
	job.Status = core.JobCanceled
	svc := &fakeService{result: sampleResult(), job: job}

	resp := get(t, newServer(t, svc, fakeModels{}), "/asr_task/job_abc123~txt")
	var got taskState
	decodeBody(t, resp, &got)
	if got.Error == nil || got.Error.Title != string(core.JobCanceled) {
		t.Errorf("state = %+v, want the terminal status named", got)
	}
}

// An unknown task and someone else's task are the same 404 with the same words:
// whose job it is must not be probeable.
func TestAsrTaskByIDIsNotFound(t *testing.T) {
	svc := &fakeService{
		result: sampleResult(),
		jobErr: core.Errorf(core.CodeJobNotFound, "no such job: job_zzz"),
	}
	resp := get(t, newServer(t, svc, fakeModels{}), "/asr_task/job_zzz~txt")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env errorEnvelope
	decodeBody(t, resp, &env)
	if env.Detail.Message != "task not found" {
		t.Errorf("message = %q, want the upstream wording", env.Detail.Message)
	}
}

func TestSplitTaskID(t *testing.T) {
	for _, tc := range []struct{ in, id, output string }{
		{"job_abc~srt", "job_abc", outputSRT},
		{"job_abc~json", "job_abc", outputJSON},
		{"job_abc", "job_abc", defaultOutput},
		{"job_abc~docx", "job_abc~docx", defaultOutput},
	} {
		id, output := splitTaskID(tc.in)
		if id != tc.id || output != tc.output {
			t.Errorf("splitTaskID(%q) = %q, %q; want %q, %q", tc.in, id, output, tc.id, tc.output)
		}
	}
}
