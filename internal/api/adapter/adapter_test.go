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

// off is registered but never asked for, which is how a dialect somebody has
// not enabled reaches the documentation page.
type off struct{}

func (off) Name() string                             { return "off" }
func (off) Mount(*http.ServeMux, core.Service, Deps) {}
func (off) Doc() Doc {
	return Doc{Title: "Off", Routes: []Route{{Method: "GET", Path: "/off"}}}
}

// Register panics on a duplicate, so the test dialects go in once for the
// whole package. Every test here reads the registry; none of them owns it.
func init() {
	Register(quiet{})
	Register(loud{})
	Register(off{})
}

func byName(docs []Doc, name string) (Doc, bool) {
	for _, d := range docs {
		if d.Name == name {
			return d, true
		}
	}
	return Doc{}, false
}

func TestDocsForDescribesTheDialectsAsked(t *testing.T) {
	docs := DocsFor([]string{"loud", "quiet", "absent"})

	if _, ok := byName(docs, "absent"); ok {
		t.Error("a dialect this build does not register is documented")
	}
	l, ok := byName(docs, "loud")
	if !ok || len(l.Routes) != 1 || !l.Enabled {
		t.Errorf("the documented dialect is wrong: %+v", l)
	}
	// A dialect that describes nothing is listed as a visible gap rather than
	// left out, which would read as "this server does not serve it".
	q, ok := byName(docs, "quiet")
	if !ok || q.Summary == "" || !q.Enabled {
		t.Errorf("an undocumented dialect is not reported: %+v", q)
	}
	// Asked for first, so described first.
	if docs[0].Name != "loud" || docs[1].Name != "quiet" {
		t.Errorf("order = %q, %q; want the configuration's order", docs[0].Name, docs[1].Name)
	}
}

// A dialect this build carries but the configuration does not name is still
// described, marked off. Leaving it out left somebody who had just read about
// an endpoint unable to tell whether it was missing from the build or merely
// switched off — and enabling it is one line of configuration.
func TestDocsForDescribesWhatIsNotEnabled(t *testing.T) {
	docs := DocsFor([]string{"loud"})

	o, ok := byName(docs, "off")
	if !ok {
		t.Fatal("a registered dialect that is not enabled is missing from the documentation")
	}
	if o.Enabled {
		t.Error("a dialect that was not asked for is marked enabled")
	}
	if o.Title == "" {
		t.Errorf("the disabled dialect carries no title: %+v", o)
	}
	// Enabled first: the page leads with what actually answers.
	if !docs[0].Enabled {
		t.Errorf("the first doc is not an enabled one: %+v", docs[0])
	}
	for i := 1; i < len(docs); i++ {
		if docs[i].Enabled && !docs[i-1].Enabled {
			t.Error("an enabled dialect is described after a disabled one")
		}
	}
}

// Naming a dialect twice must not describe it twice.
func TestDocsForDoesNotRepeatADialect(t *testing.T) {
	docs := DocsFor([]string{"loud", "loud"})

	n := 0
	for _, d := range docs {
		if d.Name == "loud" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the dialect is described %d times, want once", n)
	}
}
