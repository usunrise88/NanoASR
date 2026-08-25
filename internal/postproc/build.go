package postproc

import "github.com/usunrise88/nanoasr/internal/core"

// Factory decides which stages a request gets.
//
// It is the one place that knows both what was asked for and what this build
// can do, which is why the warnings are written here rather than in the
// pipeline: a warning produced anywhere else would be a second opinion about
// the same question, free to drift from the answer.
type Factory struct {
	// Punct is the punctuation model, or nil when none is configured.
	Punct AddPunct
	// ITNLocale is the configured locale for inverse text normalisation.
	ITNLocale string
	// PunctuationDefault and ITNDefault are the server-side switches. A request
	// can only ask for a stage the server has enabled.
	PunctuationDefault bool
	ITNDefault         bool
}

// Chain returns the stages to run and the warnings for what was asked for and
// cannot be delivered.
//
// caps is the recogniser's own capability set: a model that punctuates itself
// needs no punctuation stage, and saying "unavailable" to a caller who is about
// to receive punctuation would teach them to ignore the field.
func (f *Factory) Chain(req core.Request, caps core.Capabilities, language string) (Chain, []core.Warning) {
	var chain Chain
	var warn []core.Warning

	if f.wantsPunctuation(req) && !caps.PunctuationBuiltin {
		switch {
		case f.Punct == nil:
			warn = append(warn, core.Warning{
				Code: "punctuation_unavailable",
				Message: "this model does not punctuate and no punctuation model is configured; " +
					"choose a model that punctuates, such as gigaam-v3-ctc-punct-ru for Russian",
			})
		default:
			chain = append(chain, NewPunctuator(f.Punct))
		}
	}

	if f.wantsITN(req) {
		stage, err := NewITN(f.itnLocale(req, language))
		if err != nil {
			warn = append(warn, core.Warning{
				Code:    "itn_unavailable",
				Message: core.AsError(err).Message,
			})
		} else {
			chain = append(chain, stage)
		}
	}
	return chain, warn
}

func (f *Factory) wantsPunctuation(req core.Request) bool {
	return req.Punctuate || f.PunctuationDefault
}

func (f *Factory) wantsITN(req core.Request) bool {
	return req.ITN || f.ITNDefault
}

// itnLocale prefers the request's language, because normalisation rules are a
// property of what was spoken rather than of how the server was configured. The
// configured locale is the fallback for a request that named no language.
func (f *Factory) itnLocale(req core.Request, language string) string {
	if req.Language != "" {
		return req.Language
	}
	if language != "" {
		return language
	}
	return f.ITNLocale
}
