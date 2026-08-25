// Package itn turns spoken forms back into written ones: "двадцать пять
// рублей" becomes "25 руб.".
//
// It is a rule layer of our own rather than sherpa-onnx's RuleFsts, which have
// effectively nothing for Russian (SPEC §5.6).
//
// The contract every rule obeys: it consumes a contiguous run of words and
// produces exactly one, whose span is [start(first), end(last)] and whose
// Original keeps what was said. Nothing may reorder words, and nothing may
// produce a word covering inputs it did not consume — the word↔time link is
// what the player, the subtitles and the golden tests all depend on.
package itn

import (
	"sort"
	"strings"
	"sync"
)

// MaxGap is how long a pause may be inside one rewritten span, in seconds.
//
// Without it "двадцать" ending a sentence and "пять" beginning the next would
// become "25". A number is spoken without stopping in the middle; a silence
// long enough to be a VAD boundary is evidence that these are two thoughts.
const MaxGap = 0.35

// Token is one word as a rule sees it: the text, lowercased for matching, and
// enough timing to decide whether it may join its neighbour.
type Token struct {
	Text  string
	Lower string
	Start float64
	End   float64
}

// Match is a rule's answer: replace Count input tokens starting at the offered
// position with Text.
type Match struct {
	Text  string
	Count int
}

// Rules is one locale's rule set.
type Rules interface {
	// Locale is the BCP-47-ish tag this set answers to.
	Locale() string
	// Match is offered the remaining tokens and returns the longest rewrite
	// that begins at in[0], or ok=false to leave the word alone.
	Match(in []Token) (Match, bool)
}

var (
	mu       sync.RWMutex
	registry = map[string]Rules{}
)

// Register adds a locale. Adding a language is this call plus one file: the
// engine, the stage and the pipeline do not change.
func Register(r Rules) {
	mu.Lock()
	defer mu.Unlock()
	registry[strings.ToLower(r.Locale())] = r
}

// Get looks up a locale, falling back to the base language so that "ru-RU"
// finds "ru".
func Get(locale string) (Rules, bool) {
	mu.RLock()
	defer mu.RUnlock()

	key := strings.ToLower(strings.TrimSpace(locale))
	if r, ok := registry[key]; ok {
		return r, true
	}
	if base, _, found := strings.Cut(key, "-"); found {
		if r, ok := registry[base]; ok {
			return r, true
		}
	}
	return nil, false
}

// Locales lists what is registered, for an error message that can say what is
// available instead of only what is missing.
func Locales() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
