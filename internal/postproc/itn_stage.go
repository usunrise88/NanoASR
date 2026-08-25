package postproc

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/postproc/itn"
)

// ITN performs inverse text normalisation: "двадцать пять рублей" → "25 руб.".
//
// sherpa-onnx ships RuleFsts for some languages and effectively nothing for
// Russian, so the rules are ours (SPEC §5.6). This is the only stage that
// merges words, and the span vector is how the merge finds its way back into
// the segment it came from.
type ITN struct {
	rules  itn.Rules
	locale string
}

// NewITN looks up a locale. An unknown one is an error rather than a silent
// no-op: a caller who asked for normalisation and got none should be told.
func NewITN(locale string) (*ITN, error) {
	rules, ok := itn.Get(locale)
	if !ok {
		return nil, core.Errorf(core.CodeCapabilityUnavailable,
			"no inverse text normalisation rules for locale %q; this build has %v",
			locale, itn.Locales())
	}
	return &ITN{rules: rules, locale: rules.Locale()}, nil
}

func (i *ITN) Name() string { return "itn" }

func (i *ITN) Apply(ctx context.Context, in []core.Word) ([]core.Word, []int, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	out, spans := itn.Rewrite(i.rules, in)
	return out, spans, nil
}
