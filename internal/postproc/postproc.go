// Package postproc holds the optional text stages that run after recognition.
//
// The single rule every stage obeys: it may rewrite text, but it may never
// break the word↔time link (SPEC §5.6). A stage takes words and returns words,
// and each output word must map to a contiguous run of input words so its span
// is [start(first), end(last)].
package postproc

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/core"
)

// Stage transforms words while preserving timing.
type Stage interface {
	Name() string
	Apply(ctx context.Context, in []core.Word) ([]core.Word, error)
}

// Chain runs stages in order, stopping at the first error.
type Chain []Stage

func (c Chain) Apply(ctx context.Context, in []core.Word) ([]core.Word, error) {
	out := in
	for _, s := range c {
		var err error
		if out, err = s.Apply(ctx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Punctuator restores punctuation and capitalisation using a separate
// sherpa-onnx OfflinePunctuation model (CT-Transformer).
//
// Marks are attached to the preceding word, so word boundaries never move.
type Punctuator struct{ model string }

func NewPunctuator(model string) *Punctuator { return &Punctuator{model: model} }

func (*Punctuator) Name() string { return "punctuation" }

// TODO(M5): run the CT-Transformer over the joined text, then walk the
// punctuated output against the input words to re-attach marks.
func (*Punctuator) Apply(context.Context, []core.Word) ([]core.Word, error) {
	return nil, core.ErrNotImplemented
}

// ITN performs inverse text normalisation: "двадцать пять рублей" → "25 руб.".
//
// sherpa-onnx ships RuleFsts for some languages, but effectively nothing for
// Russian, so this is a rule layer of our own: numbers, dates, times, money,
// phone numbers, percentages. Rewritten spans keep the original surface form in
// Word.Original so a client can show either.
type ITN struct{ locale string }

func NewITN(locale string) *ITN { return &ITN{locale: locale} }

func (*ITN) Name() string { return "itn" }

// TODO(M5): implement the rule engine and a golden corpus per locale.
func (*ITN) Apply(context.Context, []core.Word) ([]core.Word, error) {
	return nil, core.ErrNotImplemented
}
