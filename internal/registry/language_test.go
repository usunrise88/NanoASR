package registry

import (
	"context"
	"testing"
)

func TestSupportsAndResolveForASingleLanguageModel(t *testing.T) {
	m := Manifest{Languages: []string{"ru"}}

	if m.IsMultilingual() {
		t.Error("a model with no locales is not multilingual")
	}
	for lang, want := range map[string]bool{
		"": true, "ru": true,
		// "choose for yourself" is something any model can honour.
		"auto": true, "AUTO": true,
		// A locale and its bare code are the same language: refusing "ru-RU"
		// on a Russian model would be a warning about nothing.
		"ru-RU": true, "RU": true, "ru_RU": true,
		"en": false, "en-US": false,
	} {
		if got := m.Supports(lang); got != want {
			t.Errorf("Supports(%q) = %v, want %v", lang, got, want)
		}
	}
	// Nothing to resolve: the request comes back untouched.
	if got, ok := m.ResolveLanguage("en"); got != "en" || !ok {
		t.Errorf("ResolveLanguage on a monolingual model = %q, %v", got, ok)
	}
}

func TestSupportsAndResolveForAMultilingualModel(t *testing.T) {
	m := Manifest{Languages: []string{"multi"}, Locales: []string{"en-US", "en-GB", "ru-RU"}}

	if !m.IsMultilingual() {
		t.Fatal("a model with locales is multilingual")
	}
	for lang, want := range map[string]bool{
		"": true, "auto": true, "ru": true, "ru-RU": true, "en-GB": true, "en": true,
		// "multi" no longer means "anything": a model that lists its locales is
		// held to them, which is what lets a bad code be warned about.
		"de-DE": false, "xx": false,
	} {
		if got := m.Supports(lang); got != want {
			t.Errorf("Supports(%q) = %v, want %v", lang, got, want)
		}
	}
	if got, _ := m.ResolveLanguage("en"); got != "en-US" {
		t.Errorf("a bare code resolves to the first locale listed, got %q", got)
	}
	if got, _ := m.ResolveLanguage(""); got != AutoLanguage {
		t.Errorf("no language resolves to auto, got %q", got)
	}
}

// A model that says "multi" and lists no locales is a supporting model or an
// older manifest: it keeps the wildcard it always had.
func TestMultiWithoutLocalesStaysAWildcard(t *testing.T) {
	m := Manifest{Languages: []string{"multi"}}
	if !m.Supports("de-DE") || m.IsMultilingual() {
		t.Error("multi without locales must stay a wildcard and not become multilingual")
	}
}

// The catalog entry is the contract the rest of the work leans on: a locale
// list whose order and contents the resolver and the documentation both
// assume, and a feature width that was read off the model rather than guessed.
func TestTheMultilingualCatalogEntry(t *testing.T) {
	entries, err := (&Local{}).Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	for _, e := range entries {
		if e.ID == "nemotron-3.5-streaming-multi" {
			m = e
		}
	}
	if m.ID == "" {
		t.Fatal("the multilingual entry is missing from the catalog")
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("the entry does not validate: %v", err)
	}
	if !m.Streaming || m.Family != "streaming_transducer" || !m.IsMultilingual() {
		t.Errorf("streaming=%v family=%q multilingual=%v", m.Streaming, m.Family, m.IsMultilingual())
	}
	if m.Features.Dim != 128 {
		t.Errorf("features.dim = %d; the encoder's own metadata says 128, and a wrong width aborts the process", m.Features.Dim)
	}
	if len(m.Locales) != 32 {
		t.Errorf("%d locales; the model card lists 32 that transcribe out of the box", len(m.Locales))
	}

	seen := map[string]bool{}
	for _, l := range m.Locales {
		if seen[l] {
			t.Errorf("locale %s is listed twice", l)
		}
		seen[l] = true
	}
	// What a bare code means is decided by this order.
	for bare, want := range map[string]string{
		"en": "en-US", "es": "es-US", "fr": "fr-FR", "pt": "pt-BR", "zh": "zh-CN", "ru": "ru-RU", "no": "nb-NO",
	} {
		if got, ok := m.ResolveLanguage(bare); !ok || got != want {
			t.Errorf("%q resolves to %q (%v), want %q", bare, got, ok, want)
		}
	}
	// Offered by the model's tokenizer but not trained to transcribe, so
	// deliberately absent: see the comment on the entry.
	for _, l := range []string{"el-GR", "lt-LT", "lv-LV", "mt-MT", "sl-SI", "he-IL", "th-TH", "nn-NO"} {
		if seen[l] {
			t.Errorf("%s is adaptation-ready only and must not be offered", l)
		}
	}
	if m.CommercialUse != CommercialYes {
		t.Errorf("commercial_use = %q", m.CommercialUse)
	}
}
