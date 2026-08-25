package asr

import (
	"bufio"
	"os"
	"strings"
	"unicode"
)

// VocabularyPunctuates reports whether a model's vocabulary can emit
// punctuation and capitalisation, by reading the vocabulary.
//
// It lives here rather than beside the loader because two callers need it and
// only one of them loads anything: the pool answers "can this model punctuate?"
// for models that are merely on disk, which is when the question is asked — a
// user picks a model before transcribing with it, not after.
//
// The alternative was a capabilities block in the manifest, which the manifest
// deliberately does not have: what a model can produce follows from the model,
// and a manifest claiming otherwise is a lie the pipeline discovers at runtime
// anyway. Here there is nothing to discover later — the tokens file is the
// model's own account of what it can output, it is already open at load time,
// and it is a few hundred lines.
//
// The signal is unambiguous in practice. GigaAM v2 has 34 tokens, lowercase
// Cyrillic letters and nothing else; the punctuating v3 has 257 including a
// full stop, a comma, a question mark and 73 tokens carrying capitals. A model
// that cannot write a full stop is not going to punctuate.
//
// An unreadable tokens file is not an error: it means the recogniser is about
// to fail to load for a better reason, and reporting "no builtin punctuation"
// on the way is the harmless answer.
func VocabularyPunctuates(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	sawSentenceMark, sawUpper := false, false

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// Each line is "<token> <id>"; the token itself may be a space, so
		// only the last field is dropped.
		line := sc.Text()
		i := strings.LastIndexByte(line, ' ')
		if i <= 0 {
			continue
		}
		tok := line[:i]

		for _, r := range tok {
			switch {
			case r == '.' || r == '?' || r == '!':
				sawSentenceMark = true
			case unicode.IsUpper(r):
				sawUpper = true
			}
		}
		if sawSentenceMark && sawUpper {
			return true
		}
	}
	// Both halves are required. A vocabulary with capitals but no sentence
	// marks is a truecasing model, and one with marks but no capitals is
	// likelier to be a stray symbol than a punctuator.
	return sawSentenceMark && sawUpper
}
