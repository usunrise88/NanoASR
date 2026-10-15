package core

import "testing"

func TestResolveLocale(t *testing.T) {
	// In the order the catalog lists them: the primary variant of a language
	// comes first, because that is what a bare code resolves to.
	locales := []string{"en-US", "en-GB", "es-US", "es-ES", "fr-FR", "fr-CA", "pt-BR", "pt-PT", "ru-RU", "nb-NO", "zh-CN"}

	for _, tc := range []struct {
		name, ask, want string
		ok              bool
	}{
		{"empty means auto", "", "auto", true},
		{"auto", "auto", "auto", true},
		{"auto is case blind", "AUTO", "auto", true},
		{"padding is forgiven", "  auto ", "auto", true},
		{"an exact locale", "ru-RU", "ru-RU", true},
		{"case is forgiven", "ru-ru", "ru-RU", true},
		{"underscore is forgiven", "pt_BR", "pt-BR", true},
		{"a bare code is its language's first locale", "pt", "pt-BR", true},
		{"a bare code, first of a pair", "en", "en-US", true},
		{"a bare code with one locale", "ru", "ru-RU", true},
		{"the second variant is still reachable", "fr-CA", "fr-CA", true},
		{"Norwegian is Bokmål", "no", "nb-NO", true},
		{"a locale the model does not list", "de-DE", "", false},
		{"a bare code it does not list", "de", "", false},
		{"nonsense", "klingon", "", false},
		{"a variant it does not list falls through", "pt-AO", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveLocale(locales, tc.ask)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ResolveLocale(%q) = %q, %v; want %q, %v", tc.ask, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBaseTag(t *testing.T) {
	for in, want := range map[string]string{
		"pt-BR": "pt", "pt_BR": "pt", "RU": "ru", " en-us ": "en", "": "", "haw-US": "haw",
	} {
		if got := BaseTag(in); got != want {
			t.Errorf("BaseTag(%q) = %q, want %q", in, got, want)
		}
	}
}
