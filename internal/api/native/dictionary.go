package native

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/usunrise88/nanoasr/internal/api/adapter"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/hotwords"
	"github.com/usunrise88/nanoasr/internal/httpx"
)

// maxDictionaryUpload bounds an imported file. A dictionary is a list of
// phrases; a megabyte of them is already two orders of magnitude past what
// biasing does anything useful with, so anything larger is a wrong file rather
// than a large dictionary.
const maxDictionaryUpload = 1 << 20

// mountDictionaries adds the hotword dictionary endpoints.
//
// Reading is open to any key and writing needs an administrative one, which is
// the same split as models and for the same reason: a dictionary changes what
// every caller's transcripts come out as, so it belongs to whoever runs the
// server rather than to whoever can reach it.
func (a *Adapter) mountDictionaries(mux *http.ServeMux, deps adapter.Deps, admin httpx.Middleware) {
	mux.HandleFunc("GET /api/v1/hotwords", a.listDictionaries(deps))
	mux.HandleFunc("GET /api/v1/hotwords/{key}", a.getDictionary(deps))

	for pattern, h := range map[string]http.HandlerFunc{
		"POST /api/v1/hotwords":              a.saveDictionary(deps, false),
		"PUT /api/v1/hotwords/{key}":         a.saveDictionary(deps, true),
		"DELETE /api/v1/hotwords/{key}":      a.deleteDictionary(deps),
		"POST /api/v1/hotwords/{key}/import": a.importDictionary(deps),
	} {
		mux.Handle(pattern, admin(http.HandlerFunc(h)))
	}
}

func dictionaryStore(deps adapter.Deps) (core.Dictionaries, error) {
	if deps.Dictionaries == nil {
		return nil, core.Errorf(core.CodeNotImplemented,
			"this server keeps no hotword dictionaries: it was built without a store")
	}
	return deps.Dictionaries, nil
}

func (*Adapter) listDictionaries(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, err := dictionaryStore(deps)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		limit, err := intValue(r, "limit")
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		list, err := store.List(r.Context(), r.URL.Query().Get("q"), limit)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		if list == nil {
			list = []core.Dictionary{}
		}
		// The policy travels with the list because every question a caller has
		// about a dictionary — will it be applied, how hard, how long may it
		// be — is answered by the server's configuration rather than by the
		// dictionary.
		writeJSON(w, http.StatusOK, map[string]any{
			"data":   list,
			"policy": deps.HotwordPolicy,
		})
	}
}

func (*Adapter) getDictionary(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, err := dictionaryStore(deps)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		d, err := store.Get(r.Context(), strings.ToLower(r.PathValue("key")))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		// text is the format a person edits and a script generates. It reads
		// back through the same import endpoint, which is the only reason to
		// have a second format at all.
		if formValue(r, "response_format") == formatText {
			writeText(w, "text/plain; charset=utf-8", hotwords.Text(d))
			return
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// dictionaryBody is what a write accepts. Phrases may arrive as a list or as
// one block of text, because a form with a textarea in it produces the second
// and nothing else would.
type dictionaryBody struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Score       float32  `json:"score"`
	Phrases     []string `json:"phrases"`
	Text        string   `json:"text"`
}

// saveDictionary creates one, or replaces one wholesale.
//
// POST without a key in the path creates and refuses to overwrite; PUT with a
// key replaces whatever is there. The difference matters for a client that is
// adding a dictionary and does not want to discover it has silently replaced
// somebody else's.
func (*Adapter) saveDictionary(deps adapter.Deps, replace bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, err := dictionaryStore(deps)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		var body dictionaryBody
		if err := json.NewDecoder(io.LimitReader(r.Body, maxDictionaryUpload)).Decode(&body); err != nil {
			WriteProblem(w, r, core.Errorf(core.CodeInvalidRequest,
				"the body must be a JSON dictionary: %v", err))
			return
		}
		if key := r.PathValue("key"); key != "" {
			body.Key = key
		}

		d := core.Dictionary{
			Key: body.Key, Name: body.Name, Description: body.Description,
			Score: body.Score, Phrases: body.Phrases,
		}
		if body.Text != "" {
			parsed, err := hotwords.Parse([]byte(body.Text))
			if err != nil {
				WriteProblem(w, r, err)
				return
			}
			d.Phrases = append(d.Phrases, parsed.Phrases...)
		}

		d, err = hotwords.Clean(d, deps.HotwordPolicy.MaxPhrases)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		if !replace {
			if _, err := store.Get(r.Context(), d.Key); err == nil {
				WriteProblem(w, r, core.Errorf(core.CodeDictionaryExists,
					"a dictionary with the key %q already exists; PUT replaces it", d.Key).
					WithParam("key"))
				return
			} else if core.AsError(err).Code != core.CodeDictionaryNotFound {
				WriteProblem(w, r, err)
				return
			}
		}

		saved, err := store.Save(r.Context(), d)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		status := http.StatusOK
		if !replace {
			status = http.StatusCreated
		}
		writeJSON(w, status, saved)
	}
}

func (*Adapter) deleteDictionary(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, err := dictionaryStore(deps)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		if err := store.Delete(r.Context(), strings.ToLower(r.PathValue("key"))); err != nil {
			WriteProblem(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// importDictionary adds the phrases in an uploaded file to a dictionary,
// creating it if the key is new.
//
// mode=append is the default because that is what uploading a file to an
// existing dictionary usually means — here are more terms — and the destructive
// reading of the same gesture should have to be asked for. mode=replace is the
// other one.
func (*Adapter) importDictionary(deps adapter.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store, err := dictionaryStore(deps)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		key, err := hotwords.ValidKey(r.PathValue("key"))
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		// The file first, so the body is read under this endpoint's own cap
		// rather than under the 32 MiB net/http buys when FormValue parses a
		// multipart body on its own.
		data, err := uploadedFile(r)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		mode := formValue(r, "mode")
		if mode == "" {
			mode = r.URL.Query().Get("mode")
		}
		if mode == "" {
			mode = "append"
		}
		if mode != "append" && mode != "replace" {
			WriteProblem(w, r, core.Errorf(core.CodeInvalidRequest,
				"mode %q is not one of append, replace", mode).WithParam("mode"))
			return
		}
		parsed, err := hotwords.Parse(data)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}

		d := core.Dictionary{Key: key, Name: parsed.Name, Description: parsed.Description,
			Score: parsed.Score, Phrases: parsed.Phrases}

		existing, err := store.Get(r.Context(), key)
		switch {
		case err == nil:
			// What the file did not say, the dictionary keeps. A file that is
			// a bare list of phrases must not blank out a name and a
			// description somebody wrote.
			if d.Name == "" {
				d.Name = existing.Name
			}
			if d.Description == "" {
				d.Description = existing.Description
			}
			if d.Score == 0 {
				d.Score = existing.Score
			}
			if mode == "append" {
				d.Phrases = append(append([]string{}, existing.Phrases...), d.Phrases...)
			}
		case core.AsError(err).Code == core.CodeDictionaryNotFound:
			// A new dictionary, named after its key until somebody renames it.
		default:
			WriteProblem(w, r, err)
			return
		}

		d, err = hotwords.Clean(d, deps.HotwordPolicy.MaxPhrases)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		saved, err := store.Save(r.Context(), d)
		if err != nil {
			WriteProblem(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	}
}

// uploadedFile reads the phrases out of either shape an upload arrives in: a
// multipart form with a file in it, as a browser and curl -F send, or the file
// as the request body, as curl --data-binary does.
func uploadedFile(r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(maxDictionaryUpload); err != nil {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"cannot read the upload: %v", err).WithParam("file")
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"the upload carries no file field").WithParam("file")
		}
		defer f.Close()
		return readCapped(f)
	}
	return readCapped(r.Body)
}

func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDictionaryUpload+1))
	if err != nil {
		return nil, core.Errorf(core.CodeInvalidRequest, "cannot read the upload: %v", err)
	}
	if len(data) > maxDictionaryUpload {
		return nil, core.Errorf(core.CodeFileTooLarge,
			"a dictionary file is at most %s", strconv.Itoa(maxDictionaryUpload/1024)+" KiB")
	}
	return data, nil
}
