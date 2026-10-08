package native

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
)

// memoryDictionaries is core.Dictionaries without a database behind it.
type memoryDictionaries struct {
	byKey map[string]core.Dictionary
}

func newMemoryDictionaries(seed ...core.Dictionary) *memoryDictionaries {
	m := &memoryDictionaries{byKey: map[string]core.Dictionary{}}
	for _, d := range seed {
		d.PhraseCount = len(d.Phrases)
		m.byKey[d.Key] = d
	}
	return m
}

func (m *memoryDictionaries) List(_ context.Context, query string, _ int) ([]core.Dictionary, error) {
	var out []core.Dictionary
	for _, d := range m.byKey {
		if query != "" && !strings.Contains(strings.ToLower(d.Key+" "+d.Name), strings.ToLower(query)) {
			continue
		}
		listed := d
		listed.Phrases = nil
		out = append(out, listed)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memoryDictionaries) Get(_ context.Context, key string) (core.Dictionary, error) {
	d, ok := m.byKey[key]
	if !ok {
		return core.Dictionary{}, core.Errorf(core.CodeDictionaryNotFound, "no such dictionary: %s", key)
	}
	return d, nil
}

func (m *memoryDictionaries) Create(ctx context.Context, d core.Dictionary) (core.Dictionary, error) {
	if _, ok := m.byKey[d.Key]; ok {
		return d, core.Errorf(core.CodeDictionaryExists,
			"a dictionary with the key %q already exists", d.Key).WithParam("key")
	}
	return m.Save(ctx, d)
}

func (m *memoryDictionaries) Save(_ context.Context, d core.Dictionary) (core.Dictionary, error) {
	d.PhraseCount = len(d.Phrases)
	m.byKey[d.Key] = d
	return d, nil
}

func (m *memoryDictionaries) Delete(_ context.Context, key string) error {
	if _, ok := m.byKey[key]; !ok {
		return core.Errorf(core.CodeDictionaryNotFound, "no such dictionary: %s", key)
	}
	delete(m.byKey, key)
	return nil
}

func dictServer(t *testing.T, store core.Dictionaries) *httptest.Server {
	t.Helper()
	return newServer(t, &fakeService{}, adapter.Deps{
		Dictionaries: store,
		HotwordPolicy: core.HotwordPolicy{
			Enabled: true, DefaultScore: 1.5, MaxVariants: 1, MaxPhrases: 100,
		},
	})
}

func send(t *testing.T, srv *httptest.Server, method, path, contentType string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestDictionaryCreateReadUpdateDelete(t *testing.T) {
	store := newMemoryDictionaries()
	srv := dictServer(t, store)

	resp := send(t, srv, http.MethodPost, "/api/v1/hotwords", "application/json",
		`{"key":"Medical","name":"Медицина","score":1.8,"phrases":["кардиомиопатия"," амиодарон ",""]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d", resp.StatusCode)
	}
	var created core.Dictionary
	decode(t, resp, &created)
	// The key is canonical whatever case it arrived in, and the blank phrase
	// is gone: what comes back is what was stored.
	if created.Key != "medical" || created.PhraseCount != 2 {
		t.Fatalf("created = %+v", created)
	}

	resp = send(t, srv, http.MethodGet, "/api/v1/hotwords/medical", "", "")
	var got core.Dictionary
	decode(t, resp, &got)
	if len(got.Phrases) != 2 || got.Phrases[1] != "амиодарон" {
		t.Errorf("phrases = %q", got.Phrases)
	}

	resp = send(t, srv, http.MethodPut, "/api/v1/hotwords/medical", "application/json",
		`{"name":"Медицина 2","phrases":["стенокардия"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replace: status = %d", resp.StatusCode)
	}
	after, _ := store.Get(context.Background(), "medical")
	if len(after.Phrases) != 1 || after.Phrases[0] != "стенокардия" || after.Name != "Медицина 2" {
		t.Errorf("after replace = %+v", after)
	}

	resp = send(t, srv, http.MethodDelete, "/api/v1/hotwords/medical", "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status = %d", resp.StatusCode)
	}
	if _, err := store.Get(context.Background(), "medical"); err == nil {
		t.Error("the dictionary survived its deletion")
	}
}

// POST creates. A POST that silently replaced an existing dictionary would let
// one team wipe another's vocabulary by picking the same obvious key.
func TestCreatingOverAnExistingDictionaryIsRefused(t *testing.T) {
	srv := dictServer(t, newMemoryDictionaries(core.Dictionary{Key: "staff", Phrases: []string{"Иванов"}}))

	resp := send(t, srv, http.MethodPost, "/api/v1/hotwords", "application/json",
		`{"key":"staff","phrases":["Петров"]}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var p problem
	decode(t, resp, &p)
	if p.Code != string(core.CodeDictionaryExists) {
		t.Errorf("code = %q", p.Code)
	}
}

func TestDictionaryListCarriesThePolicy(t *testing.T) {
	srv := dictServer(t, newMemoryDictionaries(
		core.Dictionary{Key: "a", Name: "Медицина", Phrases: []string{"x"}},
		core.Dictionary{Key: "b", Name: "Сотрудники", Phrases: []string{"y"}},
	))

	resp := send(t, srv, http.MethodGet, "/api/v1/hotwords", "", "")
	var page struct {
		Data   []core.Dictionary  `json:"data"`
		Policy core.HotwordPolicy `json:"policy"`
	}
	decode(t, resp, &page)

	if len(page.Data) != 2 {
		t.Fatalf("data = %+v", page.Data)
	}
	// Without the policy a management screen cannot tell somebody that the
	// list they are curating will not be applied.
	if !page.Policy.Enabled || page.Policy.DefaultScore != 1.5 || page.Policy.MaxPhrases != 100 {
		t.Errorf("policy = %+v", page.Policy)
	}

	resp = send(t, srv, http.MethodGet, "/api/v1/hotwords?q=медиц", "", "")
	decode(t, resp, &page)
	if len(page.Data) != 1 || page.Data[0].Key != "a" {
		t.Errorf("search returned %+v", page.Data)
	}
}

func TestImportAppendsByDefaultAndReplacesOnRequest(t *testing.T) {
	store := newMemoryDictionaries(core.Dictionary{
		Key: "staff", Name: "Сотрудники", Phrases: []string{"Иванов"},
	})
	srv := dictServer(t, store)

	resp := uploadFiles(t, srv, "/api/v1/hotwords/staff/import",
		fileField{"file", "more.txt", "# ещё сотрудники\nПетров\nСидоров\n"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import: status = %d", resp.StatusCode)
	}
	got, _ := store.Get(context.Background(), "staff")
	if strings.Join(got.Phrases, "|") != "Иванов|Петров|Сидоров" {
		t.Errorf("phrases = %q, want the upload appended", got.Phrases)
	}
	// A bare list of phrases must not blank out the name somebody gave it.
	if got.Name != "Сотрудники" {
		t.Errorf("name = %q, want it kept", got.Name)
	}

	resp = uploadFiles(t, srv, "/api/v1/hotwords/staff/import?mode=replace",
		fileField{"file", "only.txt", "Кузнецов\n"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replace import: status = %d", resp.StatusCode)
	}
	got, _ = store.Get(context.Background(), "staff")
	if strings.Join(got.Phrases, "|") != "Кузнецов" {
		t.Errorf("phrases = %q, want the upload to have replaced them", got.Phrases)
	}
}

// Uploading to a key that does not exist creates it, so a dropzone is one
// request rather than create-then-upload.
func TestImportCreatesADictionaryThatIsNotThereYet(t *testing.T) {
	store := newMemoryDictionaries()
	srv := dictServer(t, store)

	resp := uploadFiles(t, srv, "/api/v1/hotwords/products/import",
		fileField{"file", "p.csv", "phrase\nромашка\nвасилёк\n"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, err := store.Get(context.Background(), "products")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Phrases, "|") != "ромашка|василёк" {
		t.Errorf("phrases = %q, want the column header dropped", got.Phrases)
	}
}

// The raw body is the other half of the upload story: curl --data-binary and a
// script piping a file both send it this way.
func TestImportAcceptsTheFileAsTheBody(t *testing.T) {
	store := newMemoryDictionaries()
	srv := dictServer(t, store)

	resp := send(t, srv, http.MethodPost, "/api/v1/hotwords/terms/import", "text/plain",
		"ромашка\nвасилёк\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got, _ := store.Get(context.Background(), "terms")
	if got.PhraseCount != 2 {
		t.Errorf("phrases = %q", got.Phrases)
	}
}

// A dictionary exported as text has to import again, which is what makes the
// format worth having.
func TestTextExportImportsBack(t *testing.T) {
	store := newMemoryDictionaries(core.Dictionary{
		Key: "a", Name: "Имена", Description: "Из CRM", Phrases: []string{"Иванов", "Пётр Сидоров"},
	})
	srv := dictServer(t, store)

	resp := send(t, srv, http.MethodGet, "/api/v1/hotwords/a?response_format=text", "", "")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	srv2 := dictServer(t, newMemoryDictionaries())
	if resp := send(t, srv2, http.MethodPost, "/api/v1/hotwords/a/import", "text/plain", string(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-import: status = %d", resp.StatusCode)
	}
}

func TestBadDictionaryRequestsAreNamed(t *testing.T) {
	srv := dictServer(t, newMemoryDictionaries())

	for name, c := range map[string]struct {
		method, path, body string
		status             int
		param              string
	}{
		"a key with a comma in it": {
			http.MethodPost, "/api/v1/hotwords", `{"key":"a,b","phrases":["x"]}`,
			http.StatusBadRequest, "key",
		},
		"a phrase with a line break": {
			http.MethodPost, "/api/v1/hotwords", "{\"key\":\"a\",\"phrases\":[\"ро\\nма\"]}",
			http.StatusBadRequest, "phrases",
		},
		"a score nobody meant": {
			http.MethodPost, "/api/v1/hotwords", `{"key":"a","score":99,"phrases":["x"]}`,
			http.StatusBadRequest, "score",
		},
		"a dictionary that is not there": {
			http.MethodGet, "/api/v1/hotwords/absent", "",
			http.StatusNotFound, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp := send(t, srv, c.method, c.path, "application/json", c.body)
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
			var p problem
			decode(t, resp, &p)
			if c.param != "" && p.Param != c.param {
				t.Errorf("param = %q, want %q", p.Param, c.param)
			}
		})
	}
}

// A server with no store says so rather than answering an empty list, which
// would read as "there are no dictionaries" to anybody looking.
func TestWithoutAStoreTheEndpointSaysSo(t *testing.T) {
	srv := newServer(t, &fakeService{}, adapter.Deps{})
	resp := send(t, srv, http.MethodGet, "/api/v1/hotwords", "", "")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// fileField is one file in a multipart upload.
type fileField struct{ field, filename, body string }

func uploadFiles(t *testing.T, srv *httptest.Server, path string, files ...fileField) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range files {
		w, err := mw.CreateFormFile(f.field, f.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Post(srv.URL+path, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// mode travels as a form field as readily as a query parameter — a browser
// sending one multipart request has no reason to put half of it in the URL.
func TestImportReadsModeFromTheFormToo(t *testing.T) {
	store := newMemoryDictionaries(core.Dictionary{Key: "staff", Phrases: []string{"Иванов"}})
	srv := dictServer(t, store)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("mode", "replace"); err != nil {
		t.Fatal(err)
	}
	w, err := mw.CreateFormFile("file", "only.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "Кузнецов\n"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Client().Post(srv.URL+"/api/v1/hotwords/staff/import",
		mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got, _ := store.Get(context.Background(), "staff")
	if strings.Join(got.Phrases, "|") != "Кузнецов" {
		t.Errorf("phrases = %q, want the form's mode=replace honoured", got.Phrases)
	}
}

// The cap belongs on the body, not on what is read back out of it: without it
// net/http spools the whole upload to the temp directory before anyone
// measures it.
func TestAnOversizedUploadIsRefusedWithoutBeingStored(t *testing.T) {
	srv := dictServer(t, newMemoryDictionaries())

	big := strings.Repeat("ромашка\n", 400_000) // comfortably over 1 MiB
	resp := send(t, srv, http.MethodPost, "/api/v1/hotwords/big/import", "text/plain", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	var p problem
	decode(t, resp, &p)
	if p.Code != string(core.CodeFileTooLarge) {
		t.Errorf("code = %q", p.Code)
	}
}
