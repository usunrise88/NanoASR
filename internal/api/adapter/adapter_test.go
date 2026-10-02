package adapter

import (
	"net/http"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

// quiet is a dialect that does not describe itself.
type quiet struct{}

func (quiet) Name() string                             { return "quiet" }
func (quiet) Mount(*http.ServeMux, core.Service, Deps) {}

// loud describes itself.
type loud struct{}

func (loud) Name() string                             { return "loud" }
func (loud) Mount(*http.ServeMux, core.Service, Deps) {}
func (loud) Doc() Doc {
	return Doc{Title: "Loud", Routes: []Route{{Method: "GET", Path: "/loud"}}}
}

func TestDocsForDescribesOnlyTheDialectsAsked(t *testing.T) {
	Register(quiet{})
	Register(loud{})

	docs := DocsFor([]string{"loud", "quiet", "absent"})
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2 (an unknown dialect is not documented)", len(docs))
	}
	if docs[0].Name != "loud" || len(docs[0].Routes) != 1 {
		t.Errorf("the documented dialect is wrong: %+v", docs[0])
	}
	// A dialect that describes nothing is listed as a visible gap rather than
	// left out, which would read as "this server does not serve it".
	if docs[1].Name != "quiet" || docs[1].Summary == "" {
		t.Errorf("an undocumented dialect is not reported: %+v", docs[1])
	}
}
