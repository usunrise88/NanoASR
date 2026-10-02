package docs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

func testPage() Page {
	return Page{
		Version:        "v1.2.3",
		AuthMode:       "apikey",
		Public:         []string{"/healthz", "/docs"},
		UIPath:         "/ui",
		MaxUploadBytes: 1 << 30,
		Dialects: []adapter.Doc{{
			Name:    "openai",
			Title:   "OpenAI audio API",
			Summary: "Drop-in for the audio endpoints.",
			Routes: []adapter.Route{
				{Method: "POST", Path: "/v1/audio/transcriptions", Summary: "Transcribe a file."},
				{Method: "GET", Path: "/v1/models", Summary: "List models."},
			},
		}, {
			Name:  "realtime",
			Title: "Realtime",
			Routes: []adapter.Route{{
				Method: "GET", Path: "/v1/realtime?intent=transcription",
				Summary: "Open a session.",
			}},
		}},
		Server: []adapter.Route{
			{Method: "GET", Path: "/healthz", Summary: "Liveness.", Public: true},
			{Method: "GET", Path: "/api/v1/config", Summary: "Configuration.", Admin: true},
		},
		Realtime: func() *core.RealtimeInfo {
			return &core.RealtimeInfo{
				Model: "t-one-ctc-ru@2025-09-08", SampleRate: 8000,
				Languages: []string{"ru"}, MaxSessions: 4, Active: 2,
			}
		},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestThePageDescribesWhatIsMounted(t *testing.T) {
	h, err := Handler(testPage())
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/docs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type is %q", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"v1.2.3",
		"OpenAI audio API",
		"/v1/audio/transcriptions",
		"/v1/realtime?intent=transcription",
		// The loaded streaming model, with the live session count.
		"t-one-ctc-ru@2025-09-08",
		"2 of 4",
		// The admin badge, so a reader knows which key a route needs.
		"admin key",
		"/docs/openapi.json",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}

	// Both spellings of the path render, because a link to /docs/ is as likely
	// as one to /docs.
	if code := get(t, h, "/docs/").Code; code != http.StatusOK {
		t.Errorf("/docs/ answered %d", code)
	}
}

func TestThePageSaysWhenNoKeyIsNeeded(t *testing.T) {
	p := testPage()
	p.AuthMode = "open"
	h, err := Handler(p)
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/docs").Body.String()
	if !strings.Contains(body, "open mode") {
		t.Error("an open server does not say so on its reference page")
	}
	if strings.Contains(body, "Authorization: Bearer") {
		t.Error("an open server tells callers to send a bearer token")
	}
}

func TestThePageWorksWithoutAStreamingModel(t *testing.T) {
	p := testPage()
	p.Realtime = nil
	h, err := Handler(p)
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/docs").Body.String()
	if strings.Contains(body, "Streaming model") {
		t.Error("a server with no streaming model advertises one")
	}
}

func TestOpenAPIListsEveryEndpointAndNoSchemas(t *testing.T) {
	h, err := Handler(testPage())
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, "/docs/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title       string `json:"title"`
			Version     string `json:"version"`
			Description string `json:"description"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			Summary     string `json:"summary"`
			Description string `json:"description"`
			Tags        []string
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}

	if doc.OpenAPI != "3.1.0" || doc.Info.Version != "v1.2.3" {
		t.Errorf("header is wrong: %+v", doc.Info)
	}
	// The abridgement is stated rather than left to be discovered.
	if !strings.Contains(doc.Info.Description, "schemas are deliberately absent") {
		t.Errorf("the document does not admit what it leaves out: %q", doc.Info.Description)
	}

	for _, want := range []string{"/v1/audio/transcriptions", "/v1/models", "/healthz", "/api/v1/config"} {
		if _, ok := doc.Paths[want]; !ok {
			t.Errorf("%s is missing from the document", want)
		}
	}

	// A query string is not part of an OpenAPI path, and dropping it silently
	// would lose the one parameter that selects the protocol.
	op, ok := doc.Paths["/v1/realtime"]
	if !ok {
		t.Fatalf("the websocket endpoint is missing; paths are %v", keysOf(doc.Paths))
	}
	if !strings.Contains(op["get"].Description, "intent=transcription") {
		t.Errorf("the query string was dropped: %q", op["get"].Description)
	}

	if d := doc.Paths["/api/v1/config"]["get"].Description; !strings.Contains(d, "administrative") {
		t.Errorf("an admin route does not say so: %q", d)
	}
	if d := doc.Paths["/healthz"]["get"].Description; !strings.Contains(d, "without a credential") {
		t.Errorf("a public route does not say so: %q", d)
	}
	if tags := doc.Paths["/v1/models"]["get"].Tags; len(tags) != 1 || tags[0] != "openai" {
		t.Errorf("routes are not tagged with their dialect: %v", tags)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
