package hotwords

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

func TestValidKey(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string
	}{
		{"medical-terms", "medical-terms"},
		{"  Medical_Terms  ", "medical_terms"},
		{"ck2024", "ck2024"},
	} {
		got, err := ValidKey(c.in)
		if err != nil {
			t.Errorf("ValidKey(%q) = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"", "   ", "-leading", "_leading", "словарь", "with space", "a/b", "a,b", strings.Repeat("x", MaxKeyRunes+1)} {
		if _, err := ValidKey(bad); err == nil {
			t.Errorf("ValidKey(%q) was accepted", bad)
		}
	}
}

// A key travels in a comma-separated request parameter and in a URL path. Both
// of those have a character that would split it in half, and neither is
// something a caller would expect to matter.
func TestKeyCannotHoldWhatSplitsARequestParameter(t *testing.T) {
	for _, bad := range []string{"a,b", "a/b", "a b", "a?b", "a%2fb"} {
		if _, err := ValidKey(bad); err == nil {
			t.Errorf("ValidKey(%q) was accepted", bad)
		}
	}
}

func TestCleanNormalisesAndDeduplicates(t *testing.T) {
	d, err := Clean(core.Dictionary{
		Key:     "Terms",
		Phrases: []string{" ромашка ", "ромашка", "", "   ", "Иван  Петров", "Иван Петров", "кардиомиопатия"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ромашка", "Иван Петров", "кардиомиопатия"}
	if len(d.Phrases) != len(want) {
		t.Fatalf("phrases = %q, want %q", d.Phrases, want)
	}
	for i := range want {
		if d.Phrases[i] != want[i] {
			t.Fatalf("phrases = %q, want %q", d.Phrases, want)
		}
	}
	if d.PhraseCount != 3 {
		t.Errorf("PhraseCount = %d, want 3", d.PhraseCount)
	}
	// A dictionary with no name of its own is still a thing people have to
	// recognise in a list.
	if d.Name != "terms" {
		t.Errorf("Name = %q, want the key", d.Name)
	}
}

func TestCleanRefusesWhatARecogniserCannotBeGiven(t *testing.T) {
	cases := map[string]core.Dictionary{
		"a line break inside a phrase": {Key: "k", Phrases: []string{"ромашка\nвасильки"}},
		"a phrase that is a paragraph": {Key: "k", Phrases: []string{strings.Repeat("ё", MaxPhraseRunes+1)}},
		"a negative score":             {Key: "k", Score: -1},
		"a score past the sane range":  {Key: "k", Score: MaxScore + 1},
		"no key at all":                {Key: "", Phrases: []string{"ромашка"}},
	}
	for name, d := range cases {
		if _, err := Clean(d, 0); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCleanCapsThePhraseCount(t *testing.T) {
	many := make([]string, 50)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	if _, err := Clean(core.Dictionary{Key: "k", Phrases: many}, 10); err == nil {
		t.Fatal("a list past the limit was accepted")
	}
	if _, err := Clean(core.Dictionary{Key: "k", Phrases: many[:10]}, 10); err != nil {
		t.Fatalf("a list at the limit was refused: %v", err)
	}
}

func TestParseReadsTheThreeShapes(t *testing.T) {
	for name, body := range map[string]string{
		"one phrase per line":    "ромашка\nИван Петров\n",
		"with comments and gaps": "# наши термины\n\nромашка\n  # ещё\nИван Петров\n",
		"comma separated":        "ромашка, Иван Петров\n",
		"a csv column":           "phrase\nромашка\nИван Петров\n",
		"a json array":           `["ромашка", "Иван Петров"]`,
	} {
		t.Run(name, func(t *testing.T) {
			d, err := Parse([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := CleanPhrases(d.Phrases, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !contains(got, "ромашка") || !contains(got, "Иван Петров") {
				t.Errorf("phrases = %q", got)
			}
		})
	}
}

// An export has to import again: that is the whole of what makes a file format
// worth having here rather than a copy-paste box.
func TestAnExportImportsBackUnchanged(t *testing.T) {
	original, err := Clean(core.Dictionary{
		Key:         "medical",
		Name:        "Медицинские термины",
		Description: "Диагнозы и препараты\nиз карты пациента",
		Score:       1.8,
		Phrases:     []string{"кардиомиопатия", "амиодарон", "Иван Петров"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("json", func(t *testing.T) {
		body := mustJSON(t, original)
		back, err := Parse(body)
		if err != nil {
			t.Fatal(err)
		}
		if back.Key != original.Key || back.Name != original.Name || back.Score != original.Score {
			t.Errorf("metadata lost: %+v", back)
		}
		if strings.Join(back.Phrases, "|") != strings.Join(original.Phrases, "|") {
			t.Errorf("phrases = %q, want %q", back.Phrases, original.Phrases)
		}
	})

	t.Run("text", func(t *testing.T) {
		back, err := Parse([]byte(Text(original)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(back.Phrases, "|") != strings.Join(original.Phrases, "|") {
			t.Errorf("phrases = %q, want %q", back.Phrases, original.Phrases)
		}
	})
}

// The description is written into the text export as comments. A description
// that did not get commented out would come back as phrases, and the first a
// user would know of it is a recogniser biased towards its own documentation.
func TestTextExportCommentsOutEverythingThatIsNotAPhrase(t *testing.T) {
	d := core.Dictionary{
		Key:         "k",
		Name:        "Имена",
		Description: "первая строка\nвторая строка",
		Phrases:     []string{"Иванов"},
	}
	back, err := Parse([]byte(Text(d)))
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Phrases) != 1 || back.Phrases[0] != "Иванов" {
		t.Errorf("phrases = %q, want just the phrase", back.Phrases)
	}
}

func TestParseRefusesAnEmptyFile(t *testing.T) {
	for _, body := range []string{"", "   \n\n", "# only a comment\n"} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("an empty file (%q) was accepted", body)
		}
	}
}

func TestMergeKeepsOrderAndDropsRepeats(t *testing.T) {
	got := Merge([]core.Dictionary{
		{Phrases: []string{"ромашка", "василёк"}},
		{Phrases: []string{"василёк", "лютик"}},
	}, []string{"лютик", "пион"})

	want := []string{"ромашка", "василёк", "лютик", "пион"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("merged = %q, want %q", got, want)
	}
}

func TestSearchFindsPhrasesWhateverTheCase(t *testing.T) {
	phrases := []string{"Кардиомиопатия", "амиодарон", "Иванов"}
	got := Search(phrases, "кардио", 5)
	if len(got) != 1 || got[0] != "Кардиомиопатия" {
		t.Errorf("hits = %q", got)
	}
	if got := Search(phrases, "", 5); got != nil {
		t.Errorf("an empty query matched %q", got)
	}
	if got := Search(phrases, "о", 2); len(got) != 2 {
		t.Errorf("hits = %q, want the cap honoured", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, d core.Dictionary) []byte {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A CSV exported from a spreadsheet carries its column name. Importing it as a
// phrase would bias the recogniser towards the word "phrase".
func TestParseDropsASpreadsheetColumnHeader(t *testing.T) {
	d, err := Parse([]byte("phrase\nромашка\nслово\n"))
	if err != nil {
		t.Fatal(err)
	}
	if contains(d.Phrases, "phrase") {
		t.Errorf("phrases = %q, want the header gone", d.Phrases)
	}
	// Only in first position: the Russian for "word" is a word somebody may
	// well want biased, and so is the English one further down a list.
	if !contains(d.Phrases, "слово") {
		t.Errorf("phrases = %q, want the body kept", d.Phrases)
	}
	again, err := Parse([]byte("ромашка\nword\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !contains(again.Phrases, "word") {
		t.Errorf("phrases = %q, want a later \"word\" kept", again.Phrases)
	}
}
