package pipeline

import "github.com/usunrise88/nanoasr/internal/core"

// pendingFeatures reports the M5 options this build still does not act on.
//
// It is a staging area, not a design. Every entry here is owned by one M5 track
// and disappears when that track lands, moving its reporting to the stage that
// actually knows why the option could not be honoured — the acquire step for
// recogniser variants, the post-processing factory for punctuation and ITN, the
// diarize step for speakers. When this function is empty it is deleted.
//
// It exists at all so the honesty rule survives the milestone: a transcript
// that quietly ignored diarize=true is worse than one that says it did.
func pendingFeatures(req core.Request) []core.Warning {
	var out []core.Warning

	// Owned by track D (diarization).
	if req.Diarize {
		out = append(out, core.Warning{
			Code:    "diarization_unavailable",
			Message: "diarization is not implemented in this build; every word is unattributed",
		})
	}
	// Owned by track C (post-processing).
	if req.Punctuate {
		out = append(out, core.Warning{
			Code:    "punctuation_unavailable",
			Message: "punctuation restoration is not implemented in this build",
		})
	}
	if req.ITN {
		out = append(out, core.Warning{
			Code:    "itn_unavailable",
			Message: "inverse text normalisation is not implemented in this build",
		})
	}
	// Owned by track B (recogniser variants).
	if len(req.Hotwords) > 0 {
		out = append(out, core.Warning{
			Code:    "hotwords_unavailable",
			Message: "hotword biasing is not implemented in this build; the words were ignored",
		})
	}
	return out
}
