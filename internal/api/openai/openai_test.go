package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

// --- fakes ------------------------------------------------------------------

type fakeService struct {
	result *core.Result
	err    error
	got    core.Request
}

func (f *fakeService) Transcribe(_ context.Context, req core.Request) (*core.Result, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	// Copy so a test mutating warnings does not leak into the next one.
	res := *f.result
	res.Warnings = append([]core.Warning(nil), f.result.Warnings...)
	return &res, nil
}

func (f *fakeService) Submit(context.Context, core.Request) (*core.Job, error) {
	return nil, core.ErrNotImplemented
}
func (f *fakeService) Job(context.Context, string) (*core.Job, error) {
	return nil, core.ErrNotImplemented
}
func (f *fakeService) ListJobs(context.Context, core.JobFilter) ([]core.Job, error) {
	return nil, core.ErrNotImplemented
}
func (f *fakeService) Cancel(context.Context, string) error { return core.ErrNotImplemented }
func (f *fakeService) Watch(context.Context, string) (<-chan core.Job, error) {
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
		ID: "txn_1", Model: "gigaam@1", Language: "ru", Duration: 3.5,
		Text:            "привет мир",
		TimestampSource: core.TimestampToken,
		Segments: []core.Segment{{
			ID: 0, Start: 0.5, End: 2.0, Text: "привет мир", Speaker: &speaker,
			AvgConfidence: 0.81,
			Words: []core.Word{
				{Word: "привет", Start: 0.5, End: 1.1, Confidence: 0.9},
				{Word: "мир", Start: 1.2, End: 2.0, Confidence: 0.72},
			},
		}},
		Silence: []core.Silence{{Start: 0, End: 0.5}, {Start: 2.0, End: 3.5}},
		Stats:   core.Stats{AudioDuration: 3.5, RTF: 0.12, ProcessingMS: 420},
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

type field struct{ key, value string }

func postTranscription(t *testing.T, srv *httptest.Server, headers map[string]string, fields ...field) *http.Response {
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

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/audio/transcriptions", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

// --- tests ------------------------------------------------------------------

// response_format=json is the default and must carry the text and nothing
// else: clients index straight into it.
func TestTranscriptionsDefaultFormatIsBareText(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := postTranscription(t, newServer(t, svc, fakeModels{}), nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got map[string]any
	decodeBody(t, resp, &got)

	if got["text"] != "привет мир" {
		t.Errorf("text = %v", got["text"])
	}
	if len(got) != 1 {
		t.Errorf("json format returned %d keys %v, want only text", len(got), got)
	}
}

func TestTranscriptionsVerboseJSON(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := postTranscription(t, newServer(t, svc, fakeModels{}),
		nil, field{"response_format", "verbose_json"})

	var got verboseResponse
	decodeBody(t, resp, &got)

	if got.Task != "transcribe" || got.Language != "ru" {
		t.Errorf("task/language = %q/%q", got.Task, got.Language)
	}
	if len(got.Segments) != 1 || len(got.Words) != 2 {
		t.Fatalf("got %d segments and %d words, want 1 and 2", len(got.Segments), len(got.Words))
	}
	if got.Words[0].Word != "привет" || got.Words[0].Start != 0.5 {
		t.Errorf("first word = %+v", got.Words[0])
	}
	// The fields that let a caller tell real timings from a fallback.
	if got.TimestampSource != core.TimestampToken {
		t.Errorf("timestamp_source = %q", got.TimestampSource)
	}
	if len(got.Silence) != 2 {
		t.Errorf("silence = %+v, want the two gaps around the utterance", got.Silence)
	}
	if got.Stats == nil || got.Stats.RTF == 0 {
		t.Error("stats should be reported in verbose_json")
	}
	// Speaker travels down to the word so a caller need not join by time.
	if got.Words[0].Speaker == nil || *got.Words[0].Speaker != "spk_0" {
		t.Errorf("word speaker = %v", got.Words[0].Speaker)
	}
}

func TestTranscriptionsWordGranularity(t *testing.T) {
	cases := []struct {
		name      string
		fields    []field
		wantWords bool
	}{
		{"verbose defaults to words", []field{{"response_format", "verbose_json"}}, true},
		{"segment only", []field{{"response_format", "verbose_json"}, {"timestamp_granularities[]", "segment"}}, false},
		{"word explicitly", []field{{"response_format", "verbose_json"}, {"timestamp_granularities[]", "word"}}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &fakeService{result: sampleResult()}
			resp := postTranscription(t, newServer(t, svc, fakeModels{}), nil, c.fields...)

			var got verboseResponse
			decodeBody(t, resp, &got)

			if hasWords := len(got.Words) > 0; hasWords != c.wantWords {
				t.Errorf("words present = %v, want %v", hasWords, c.wantWords)
			}
			if len(got.Segments) == 0 {
				t.Error("segments should always be present in verbose_json")
			}
		})
	}
}

func TestTranscriptionsTextAndSubtitleFormats(t *testing.T) {
	cases := []struct {
		format      string
		contentType string
		contains    []string
	}{
		{"text", "text/plain", []string{"привет мир"}},
		{"srt", "application/x-subrip", []string{"1\n", "00:00:00,500 --> 00:00:02,000", "привет мир"}},
		{"vtt", "text/vtt", []string{"WEBVTT", "00:00:00.500 --> 00:00:02.000"}},
	}

	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			svc := &fakeService{result: sampleResult()}
			resp := postTranscription(t, newServer(t, svc, fakeModels{}),
				nil, field{"response_format", c.format})

			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, c.contentType) {
				t.Errorf("Content-Type = %q, want %q", ct, c.contentType)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.contains {
				if !strings.Contains(string(raw), want) {
					t.Errorf("body does not contain %q:\n%s", want, raw)
				}
			}
		})
	}
}

func TestTranscriptionsMapsPromptToHotwords(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := postTranscription(t, newServer(t, svc, fakeModels{}), nil,
		field{"response_format", "verbose_json"},
		field{"prompt", "ромашка, Иванов ,  "})

	if len(svc.got.Hotwords) != 2 || svc.got.Hotwords[0] != "ромашка" || svc.got.Hotwords[1] != "Иванов" {
		t.Errorf("hotwords = %q", svc.got.Hotwords)
	}

	var got verboseResponse
	decodeBody(t, resp, &got)
	if !hasWarning(got.Warnings, "prompt_mapped_to_hotwords") {
		t.Errorf("the divergence from OpenAI must be reported, got %+v", got.Warnings)
	}
}

func TestTranscriptionsWarnsThatTemperatureIsIgnored(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	resp := postTranscription(t, newServer(t, svc, fakeModels{}), nil,
		field{"response_format", "verbose_json"}, field{"temperature", "0.7"})

	var got verboseResponse
	decodeBody(t, resp, &got)
	if !hasWarning(got.Warnings, "temperature_ignored") {
		t.Errorf("warnings = %+v, want temperature_ignored", got.Warnings)
	}
}

func TestTranscriptionsPassesStrictHeaderThrough(t *testing.T) {
	svc := &fakeService{result: sampleResult()}
	postTranscription(t, newServer(t, svc, fakeModels{}),
		map[string]string{"X-NanoASR-Strict": "1"})

	if !svc.got.Strict {
		t.Error("X-NanoASR-Strict: 1 did not reach the service")
	}
}

func TestTranscriptionsRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name       string
		fields     []field
		omitFile   bool
		wantStatus int
		wantCode   string
	}{
		{
			name: "unknown response format", fields: []field{{"response_format", "yaml"}},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request",
		},
		{
			name: "non-numeric temperature", fields: []field{{"temperature", "warm"}},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request",
		},
		{
			name: "bad granularity", fields: []field{{"timestamp_granularities[]", "phoneme"}},
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := &fakeService{result: sampleResult()}
			resp := postTranscription(t, newServer(t, svc, fakeModels{}), nil, c.fields...)

			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			var env errorEnvelope
			decodeBody(t, resp, &env)
			if env.Error.Code != c.wantCode {
				t.Errorf("error code = %q, want %q", env.Error.Code, c.wantCode)
			}
			if env.Error.Type != "invalid_request_error" {
				t.Errorf("error type = %q, want invalid_request_error", env.Error.Type)
			}
		})
	}
}

func TestTranscriptionsRequiresAFile(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", "gigaam")
	_ = mw.Close()

	srv := newServer(t, &fakeService{result: sampleResult()}, fakeModels{})
	resp, err := srv.Client().Post(srv.URL+"/v1/audio/transcriptions", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env errorEnvelope
	decodeBody(t, resp, &env)
	if env.Error.Param != "file" {
		t.Errorf("error should name the missing param, got %+v", env.Error)
	}
}

// Domain errors from the service keep their status and their OpenAI shape.
func TestTranscriptionsPropagatesServiceErrors(t *testing.T) {
	svc := &fakeService{err: core.Errorf(core.CodeModelNotFound, "no such model: ghost")}
	resp := postTranscription(t, newServer(t, svc, fakeModels{}), nil)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env errorEnvelope
	decodeBody(t, resp, &env)
	if env.Error.Code != "model_not_found" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestModelsList(t *testing.T) {
	models := fakeModels{infos: []core.ModelInfo{{
		ID: "gigaam-v2-ctc-ru", Revision: "1", Family: "nemo_ctc",
		Languages: []string{"ru"}, State: core.ModelReady, License: "apache-2.0",
		Capabilities: core.Capabilities{WordTimestamps: true},
	}}}
	srv := newServer(t, &fakeService{result: sampleResult()}, models)

	resp, err := srv.Client().Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got modelList
	decodeBody(t, resp, &got)
	if got.Object != "list" || len(got.Data) != 1 {
		t.Fatalf("got %+v", got)
	}
	m := got.Data[0]
	if m.ID != "gigaam-v2-ctc-ru" || m.Object != "model" || m.OwnedBy != "nanoasr" {
		t.Errorf("model object = %+v", m)
	}
	if !m.WordTimestamps || m.State != core.ModelReady {
		t.Errorf("NanoASR fields missing from %+v", m)
	}
}

func TestModelLookupUnknownIs404(t *testing.T) {
	srv := newServer(t, &fakeService{result: sampleResult()}, fakeModels{})

	resp, err := srv.Client().Get(srv.URL + "/v1/models/ghost")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTranslationsIsHonestlyUnsupported(t *testing.T) {
	srv := newServer(t, &fakeService{result: sampleResult()}, fakeModels{})

	resp, err := srv.Client().Post(srv.URL+"/v1/audio/translations", "multipart/form-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

func TestTimecodeFormatting(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{0, "00:00:00,000"},
		{0.5, "00:00:00,500"},
		{61.25, "00:01:01,250"},
		{3661.007, "01:01:01,007"},
		{-1, "00:00:00,000"},
	}
	for _, c := range cases {
		if got := timecode(c.seconds, ','); got != c.want {
			t.Errorf("timecode(%.3f) = %q, want %q", c.seconds, got, c.want)
		}
	}
}

func hasWarning(ws []core.Warning, code string) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}
