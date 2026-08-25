package asr

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVocabularyPunctuates(t *testing.T) {
	cases := []struct {
		name   string
		tokens string
		want   bool
	}{
		{
			// GigaAM v2's shape: lowercase Cyrillic and nothing else.
			name:   "a plain character vocabulary does not punctuate",
			tokens: " 0\nа 1\nб 2\nв 3\nг 4\n",
			want:   false,
		},
		{
			// GigaAM v3 punct's shape, trimmed: marks and capitals together.
			name:   "marks and capitals together mean it punctuates",
			tokens: "<unk> 0\n▁ 1\n. 2\nе 3\n, 7\n? 46\nА 100\n",
			want:   true,
		},
		{
			// Capitals alone are truecasing, which is not the question asked.
			name:   "capitals without sentence marks are not enough",
			tokens: "<unk> 0\nа 1\nА 2\nБ 3\n",
			want:   false,
		},
		{
			// A stray symbol in an otherwise lowercase vocabulary should not
			// promote the model to a punctuator.
			name:   "sentence marks without capitals are not enough",
			tokens: "<unk> 0\nа 1\n. 2\n",
			want:   false,
		},
		{
			// Punctuation can be glued to a subword piece rather than standing
			// alone, and that still counts.
			name:   "marks attached to a subword count",
			tokens: "<unk> 0\n▁да. 1\nСлово 2\n",
			want:   true,
		},
		{
			name:   "a blank file is not a punctuator",
			tokens: "",
			want:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens.txt")
			if err := os.WriteFile(path, []byte(c.tokens), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := VocabularyPunctuates(path); got != c.want {
				t.Errorf("tokensCarryPunctuation = %v, want %v", got, c.want)
			}
		})
	}
}

// A missing file means the recogniser is about to fail to load for a better
// reason. Reporting no builtin punctuation on the way must not panic.
func TestVocabularyPunctuatesMissingFile(t *testing.T) {
	if VocabularyPunctuates(filepath.Join(t.TempDir(), "absent.txt")) {
		t.Error("an unreadable tokens file must not claim builtin punctuation")
	}
}

// The token itself may be a space, so only the trailing id is stripped.
func TestVocabularyPunctuatesHandlesSpaceToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.txt")
	if err := os.WriteFile(path, []byte("  0\n. 1\nА 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !VocabularyPunctuates(path) {
		t.Error("a vocabulary whose first token is a space was misparsed")
	}
}
