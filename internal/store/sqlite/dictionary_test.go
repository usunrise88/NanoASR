package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/core"
)

func dictionaries(t *testing.T) *DictionaryStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.Dictionaries()
}

func TestDictionaryRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)

	saved, err := d.Save(ctx, core.Dictionary{
		Key:         "medical",
		Name:        "Медицинские термины",
		Description: "Из карт пациентов",
		Score:       1.8,
		Phrases:     []string{"кардиомиопатия", "амиодарон", "Иван Петров"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.PhraseCount != 3 || saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Fatalf("saved = %+v", saved)
	}

	got, err := d.Get(ctx, "medical")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Медицинские термины" || got.Score != 1.8 {
		t.Errorf("metadata = %+v", got)
	}
	// Order is the author's and has to survive the round trip, because a
	// dictionary is edited as a list and a reordered one reads as changed.
	if strings.Join(got.Phrases, "|") != "кардиомиопатия|амиодарон|Иван Петров" {
		t.Errorf("phrases = %q", got.Phrases)
	}
}

func TestSavingAgainReplacesThePhrasesAndKeepsTheBirthday(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)

	first, err := d.Save(ctx, core.Dictionary{Key: "k", Name: "one", Phrases: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}

	second, err := d.Save(ctx, core.Dictionary{Key: "k", Name: "two", Phrases: []string{"z"}})
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt moved: %v → %v", first.CreatedAt, second.CreatedAt)
	}

	got, err := d.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	// The old phrases have to be gone, not merged: a replacement that quietly
	// kept them would make removing a phrase impossible.
	if strings.Join(got.Phrases, "|") != "z" || got.Name != "two" {
		t.Errorf("after replacement: %+v", got)
	}
}

func TestSearchMatchesRussianPhrasesWhateverTheCase(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)

	if _, err := d.Save(ctx, core.Dictionary{
		Key: "medical", Name: "Медицина",
		Phrases: []string{"Кардиомиопатия", "амиодарон"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Save(ctx, core.Dictionary{
		Key: "staff", Name: "Сотрудники", Phrases: []string{"Иванов", "Петров"},
	}); err != nil {
		t.Fatal(err)
	}

	// Upper case in the query, lower in the data and the reverse: SQLite's own
	// LIKE folds ASCII only, which is why the folded columns exist.
	for _, q := range []string{"КАРДИО", "кардио", "Кардиомиопатия"} {
		got, err := d.List(ctx, q, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Key != "medical" {
			t.Fatalf("search %q returned %d dictionaries", q, len(got))
		}
		// A search result says why it matched.
		if len(got[0].Matches) != 1 || got[0].Matches[0] != "Кардиомиопатия" {
			t.Errorf("matches = %q", got[0].Matches)
		}
	}

	// The name is searchable too, and matching on it alone reports no phrases.
	byName, err := d.List(ctx, "сотрудник", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byName) != 1 || byName[0].Key != "staff" {
		t.Fatalf("name search returned %+v", byName)
	}
}

// A listing is a list of names: carrying every phrase of every dictionary
// would make the management screen the heaviest page in the product.
func TestListLeavesThePhrasesOutButCountsThem(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)
	if _, err := d.Save(ctx, core.Dictionary{Key: "k", Phrases: []string{"a", "b", "c"}}); err != nil {
		t.Fatal(err)
	}

	got, err := d.List(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("list = %+v", got)
	}
	if got[0].Phrases != nil {
		t.Errorf("phrases = %q, want them withheld from a listing", got[0].Phrases)
	}
	if got[0].PhraseCount != 3 {
		t.Errorf("PhraseCount = %d, want 3", got[0].PhraseCount)
	}
}

func TestDeleteTakesThePhrasesWithIt(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)
	if _, err := d.Save(ctx, core.Dictionary{Key: "k", Phrases: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, "k"); core.AsError(err).Code != core.CodeDictionaryNotFound {
		t.Errorf("Get after delete = %v, want dictionary_not_found", err)
	}
	// Recreating the same key must not trip over orphaned phrases.
	again, err := d.Save(ctx, core.Dictionary{Key: "k", Phrases: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if again.PhraseCount != 1 {
		t.Errorf("recreated dictionary holds %d phrases", again.PhraseCount)
	}
}

func TestMissingDictionaryIsReportedByCode(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)
	if _, err := d.Get(ctx, "absent"); core.AsError(err).Code != core.CodeDictionaryNotFound {
		t.Errorf("Get = %v, want dictionary_not_found", err)
	}
	if err := d.Delete(ctx, "absent"); core.AsError(err).Code != core.CodeDictionaryNotFound {
		t.Errorf("Delete = %v, want dictionary_not_found", err)
	}
}

// Search text that happens to be SQL wildcards must be matched literally.
// instr() is what makes that true, and this is the test that would catch a
// change back to LIKE without an escape.
func TestSearchTreatsWildcardsAsText(t *testing.T) {
	ctx := context.Background()
	d := dictionaries(t)
	if _, err := d.Save(ctx, core.Dictionary{Key: "a", Phrases: []string{"ромашка"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Save(ctx, core.Dictionary{Key: "b", Phrases: []string{"100% хлопок"}}); err != nil {
		t.Fatal(err)
	}

	got, err := d.List(ctx, "%", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "b" {
		t.Errorf("searching for %% returned %d dictionaries, want only the one containing it", len(got))
	}
}
