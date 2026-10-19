package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxSpeakerName bounds what a person may call a speaker: the name labels
// transcript lines and, for an isolated voice, a file.
const MaxSpeakerName = 64

// SpeakerName cleans a name a person gave a speaker — control characters
// dropped, space trimmed — and refuses one that is too long. An empty name is
// valid: it gives the speaker its id back.
func SpeakerName(name string) (string, error) {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name))
	if utf8.RuneCountInString(name) > MaxSpeakerName {
		return "", Errorf(CodeInvalidRequest, "a speaker's name is at most %d characters", MaxSpeakerName)
	}
	return name, nil
}
