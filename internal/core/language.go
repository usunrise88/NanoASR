package core

import "strings"

// AutoLanguage is the request value that asks a multilingual model to choose
// the language itself.
const AutoLanguage = "auto"

// ResolveLocale turns what a client asked for into one of a model's locales.
// It reports false for a language the model does not offer.
//
// "" and "auto" both mean "let the model choose" and come back as "auto", so
// nothing downstream has to tell them apart. Spelling is forgiven (case, "_"
// for "-"), and a bare code resolves to the first locale listed for that
// language, because what a model means by "pt" is not something a caller can be
// expected to know — the list is ordered with the primary variant first.
// "no" is the Norwegian macrolanguage and means Bokmål.
//
// It lives here because the file pipeline and the realtime dialect both need
// the answer, and two copies of it would drift.
func ResolveLocale(locales []string, req string) (string, bool) {
	want := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(req), "_", "-"))
	if want == "" || want == AutoLanguage {
		return AutoLanguage, true
	}
	if want == "no" {
		want = "nb"
	}
	for _, l := range locales {
		if strings.ToLower(l) == want {
			return l, true
		}
	}
	if !strings.Contains(want, "-") {
		for _, l := range locales {
			if BaseTag(l) == want {
				return l, true
			}
		}
	}
	return "", false
}

// BaseTag is the language part of a BCP-47-ish tag, lower-cased: "pt-BR" → "pt".
func BaseTag(tag string) string {
	tag = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"))
	if i := strings.IndexByte(tag, '-'); i > 0 {
		return tag[:i]
	}
	return tag
}
