// Package hotwords holds everything about a hotword dictionary that is neither
// storage nor transport: what a key may look like, what a phrase list is
// allowed to contain, and how a file of phrases is read and written.
//
// It is separate from internal/asr, which knows how a list is rendered for one
// particular recogniser, and from the store, which only persists. The rules
// here are the ones a person meets — a rejected key, a phrase that is too long,
// a file in a format nobody declared — so they are in one place and tested as
// such.
package hotwords

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/usunrise88/nanoasr/internal/core"
)

// The limits. Each is a number somebody has to be told when they hit it, so
// each is named rather than written into a condition.
const (
	// MaxKeyRunes is generous for a name meant to be typed into a request.
	MaxKeyRunes = 64
	// MaxNameRunes and MaxDescriptionRunes bound what the UI has to render.
	MaxNameRunes        = 200
	MaxDescriptionRunes = 2000
	// MaxPhraseRunes bounds one entry. A hotword is a word or a short phrase;
	// anything longer is a sentence somebody pasted, and biasing towards a
	// whole sentence does nothing useful.
	MaxPhraseRunes = 200
	// DefaultMaxPhrases is the ceiling when the configuration does not set
	// one. Accuracy gives out long before this does — the longer the list, the
	// more often the bias fires in the wrong place — so it is a guard against
	// a runaway import, not a recommendation.
	DefaultMaxPhrases = 5000
	// MaxScore is a sanity bound. 1.5–2.0 is the working range; 10 already
	// makes a recogniser repeat the list back instead of transcribing, which
	// is measurable, so anything above it is a typo rather than a choice.
	MaxScore = 10
)

// ValidKey reports the canonical form of a dictionary key, or why there is
// none.
//
// Lowercase, digits, dash and underscore: the key travels in URLs, in form
// fields and in comma-separated lists of keys, and each of those has its own
// opinion about spaces, commas and slashes. Taking the intersection once here
// means a key that works in one place works in all of them.
func ValidKey(key string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return "", core.Errorf(core.CodeInvalidRequest,
			"a dictionary needs a key").WithParam("key")
	}
	if utf8.RuneCountInString(k) > MaxKeyRunes {
		return "", core.Errorf(core.CodeInvalidRequest,
			"the key is longer than %d characters", MaxKeyRunes).WithParam("key")
	}
	for i, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return "", core.Errorf(core.CodeInvalidRequest,
				"the key %q may hold lowercase latin letters, digits, dashes and "+
					"underscores, and must start with a letter or a digit", key).
				WithParam("key")
		}
	}
	return k, nil
}

// Clean validates a dictionary and returns it in the form that is stored:
// canonical key, trimmed text, and a phrase list with the blanks and the
// repetitions taken out.
//
// Order is kept. A phrase list is written by a person in an order that means
// something to them, and sorting it would make every later diff unreadable for
// no gain — sherpa-onnx reads the list into a graph and does not care.
func Clean(d core.Dictionary, maxPhrases int) (core.Dictionary, error) {
	if maxPhrases <= 0 {
		maxPhrases = DefaultMaxPhrases
	}

	key, err := ValidKey(d.Key)
	if err != nil {
		return d, err
	}
	d.Key = key

	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		d.Name = d.Key
	}
	if utf8.RuneCountInString(d.Name) > MaxNameRunes {
		return d, core.Errorf(core.CodeInvalidRequest,
			"the name is longer than %d characters", MaxNameRunes).WithParam("name")
	}
	d.Description = strings.TrimSpace(d.Description)
	if utf8.RuneCountInString(d.Description) > MaxDescriptionRunes {
		return d, core.Errorf(core.CodeInvalidRequest,
			"the description is longer than %d characters", MaxDescriptionRunes).
			WithParam("description")
	}

	if d.Score < 0 || d.Score > MaxScore {
		return d, core.Errorf(core.CodeInvalidRequest,
			"score must be between 0 and %d, got %v (0 means the server default)",
			MaxScore, d.Score).WithParam("score")
	}

	phrases, err := CleanPhrases(d.Phrases, maxPhrases)
	if err != nil {
		return d, err
	}
	d.Phrases = phrases
	d.PhraseCount = len(phrases)
	return d, nil
}

// CleanPhrases trims, drops the empties and the repeats, and refuses what
// cannot be sent to a recogniser.
func CleanPhrases(in []string, maxPhrases int) ([]string, error) {
	if maxPhrases <= 0 {
		maxPhrases = DefaultMaxPhrases
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))

	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		// A line break would become two hotwords in the file sherpa-onnx
		// reads, one of them half a phrase. The same refusal lives in
		// asr.Hotwords.Buffer, for lists that never came through a dictionary.
		if strings.ContainsAny(p, "\n\r") {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"the phrase %q contains a line break", p).WithParam("phrases")
		}
		if utf8.RuneCountInString(p) > MaxPhraseRunes {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"a phrase is longer than %d characters: %q", MaxPhraseRunes, truncate(p, 40)).
				WithParam("phrases")
		}
		// Collapse runs of whitespace: "Иван  Петров" and "Иван Петров" are
		// the same phrase to a tokeniser, and keeping both would spend a slot
		// on a duplicate the author cannot see.
		p = strings.Join(strings.FieldsFunc(p, unicode.IsSpace), " ")
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) > maxPhrases {
			return nil, core.Errorf(core.CodeInvalidRequest,
				"a dictionary holds at most %d phrases", maxPhrases).WithParam("phrases")
		}
	}
	return out, nil
}

// Parse reads an uploaded file into a dictionary.
//
// Three shapes, told apart by reading the bytes rather than by trusting the
// name an upload arrived with:
//
//   - a JSON array of strings, which is the shape a client already has if it
//     was sending hotwords[] inline;
//   - a JSON object, which is what this server exports, so an export imports
//     again unchanged;
//   - anything else: one phrase per line, blank lines ignored, "#" starting a
//     comment, and commas separating phrases on a line — which makes a
//     one-column CSV and a comma-separated list the same file as far as this
//     is concerned.
//
// What comes back carries only what the file said. The caller decides what the
// key is when the file did not name one.
func Parse(data []byte) (core.Dictionary, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return core.Dictionary{}, core.Errorf(core.CodeInvalidRequest,
			"the file holds no phrases").WithParam("file")
	}

	switch trimmed[0] {
	case '[':
		var phrases []string
		if err := json.Unmarshal([]byte(trimmed), &phrases); err != nil {
			return core.Dictionary{}, core.Errorf(core.CodeInvalidRequest,
				"the file starts like a JSON array but does not parse as one: %v", err).
				WithParam("file")
		}
		return core.Dictionary{Phrases: phrases}, nil
	case '{':
		var d core.Dictionary
		if err := json.Unmarshal([]byte(trimmed), &d); err != nil {
			return core.Dictionary{}, core.Errorf(core.CodeInvalidRequest,
				"the file starts like a JSON object but does not parse as a dictionary: %v", err).
				WithParam("file")
		}
		return d, nil
	}

	var phrases []string
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, part := range strings.Split(line, ",") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			// A spreadsheet exports its column name with the column. Keeping
			// it would bias a recogniser towards the word "phrase", which is
			// the kind of thing nobody notices until it appears in a
			// transcript.
			if len(phrases) == 0 && isColumnHeader(p) {
				continue
			}
			phrases = append(phrases, p)
		}
	}
	if len(phrases) == 0 {
		return core.Dictionary{}, core.Errorf(core.CodeInvalidRequest,
			"the file holds no phrases").WithParam("file")
	}
	return core.Dictionary{Phrases: phrases}, nil
}

// isColumnHeader reports whether a first line is a spreadsheet's column name
// rather than a phrase. Only the names a column of hotwords plausibly carries,
// and only in first position: "word" further down the file is a word.
func isColumnHeader(s string) bool {
	switch strings.ToLower(s) {
	case "phrase", "phrases", "word", "words", "hotword", "hotwords", "term", "terms", "text":
		return true
	}
	return false
}

// Text renders a dictionary's phrases as the plain file Parse reads back: the
// header names it, and "#" makes those lines comments rather than phrases.
func Text(d core.Dictionary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", d.Key)
	if d.Name != "" && d.Name != d.Key {
		fmt.Fprintf(&b, "# %s\n", d.Name)
	}
	for _, line := range strings.Split(d.Description, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(&b, "# %s\n", line)
		}
	}
	for _, p := range d.Phrases {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	return b.String()
}

// Merge collects the phrases of several dictionaries into one list, in the
// order the caller named them, without repeats.
//
// The order matters only for what a reader sees; what it buys is that a
// request naming the same two dictionaries twice does not build two different
// recogniser variants of the same thing.
func Merge(dicts []core.Dictionary, extra []string) []string {
	out := make([]string, 0, len(extra))
	seen := map[string]bool{}
	add := func(phrases []string) {
		for _, p := range phrases {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, d := range dicts {
		add(d.Phrases)
	}
	add(extra)
	return out
}

// Search reports the phrases of a dictionary that contain the query, folded to
// lower case, capped at most.
func Search(phrases []string, query string, most int) []string {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var hits []string
	for _, p := range phrases {
		if strings.Contains(strings.ToLower(p), q) {
			hits = append(hits, p)
			if len(hits) == most {
				break
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return len(hits[i]) < len(hits[j]) })
	return hits
}

func truncate(s string, runes int) string {
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	return string(r[:runes]) + "…"
}
