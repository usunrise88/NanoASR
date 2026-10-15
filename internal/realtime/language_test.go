package realtime

import (
	"slices"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/core"
)

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The language a session opens with is handed to the stream before any audio,
// so the first chunk is already decoded for it.
func TestASessionOpensWithItsLanguage(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 2, Locales: []string{"ru-RU", "en-US"}})
	open(t, e, core.RealtimeParams{AutoTurns: true, Language: "ru-RU"})

	if got := rec.toldLanguages(); !slices.Equal(got, []string{"ru-RU"}) {
		t.Errorf("stream was told %v, want [ru-RU]", got)
	}
}

// A model with one language is never told anything: the option means nothing
// to it, and a session that named one has already been warned by the dialect.
func TestASessionWithoutALanguageTellsTheStreamNothing(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 2})
	open(t, e, core.RealtimeParams{AutoTurns: true})

	if got := rec.toldLanguages(); len(got) != 0 {
		t.Errorf("stream was told %v", got)
	}
}

// The stream belongs to the loop, so a change arrives as a request the loop
// applies — and is applied again after an utterance ends, because the model
// reads the option for every chunk and nothing promises it outlives a Reset.
func TestChangingTheLanguageReachesTheStreamAndSurvivesAnUtterance(t *testing.T) {
	rec := newFake()
	rec.endpointAfter = 2
	e := start(t, rec, Options{MaxSessions: 2, Locales: []string{"ru-RU", "en-US"}})
	s := open(t, e, core.RealtimeParams{AutoTurns: true, Language: "ru-RU"})

	s.SetLanguage("en-US")
	waitUntil(t, "the change to reach the stream", func() bool {
		return slices.Contains(rec.toldLanguages(), "en-US")
	})

	before := len(rec.toldLanguages())
	speak(t, s, 16000, 0.6) // long enough to end at least one utterance
	collect(t, s, core.RealtimeFinal)

	got := rec.toldLanguages()
	if len(got) <= before || got[len(got)-1] != "en-US" {
		t.Errorf("told %v; after the utterance ended the stream must be told en-US again", got)
	}
}

func TestChangingTheLanguageOfAClosedSessionIsHarmless(t *testing.T) {
	rec := newFake()
	e := start(t, rec, Options{MaxSessions: 2, Locales: []string{"ru-RU"}})
	s := open(t, e, core.RealtimeParams{AutoTurns: true})
	_ = s.Close()
	s.SetLanguage("ru-RU") // must neither panic nor block
}

func TestTheSessionDescriptionCarriesTheLocales(t *testing.T) {
	e := start(t, newFake(), Options{MaxSessions: 1, Locales: []string{"ru-RU", "en-US"}})
	if got := e.Describe().Locales; !slices.Equal(got, []string{"ru-RU", "en-US"}) {
		t.Errorf("locales = %v", got)
	}
}
