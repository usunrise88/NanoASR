package pipeline

import (
	"context"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// acquire leases a recogniser, taking a variant when the request asks for
// settings the base instance was not built with.
//
// A variant is never required for a correct answer, only for a better one, so
// every reason it cannot be built degrades to the base instance plus a warning
// naming the reason. The alternative — failing the request — would turn an
// optional bias list into a hard dependency on a memory budget.
func (p *Pipeline) acquire(ctx context.Context, modelID string, req core.Request) (*pool.Lease, []core.Warning, error) {
	if !wantsVariant(req) {
		lease, err := p.models.Acquire(ctx, modelID)
		return lease, nil, err
	}

	// The manifest decides whether the request is even expressible, and it is
	// readable without loading anything.
	man, err := p.models.Manifest(ctx, modelID)
	if err != nil {
		return nil, nil, err
	}

	v, warn := p.buildVariant(req, man)
	if v.Zero() {
		lease, err := p.models.Acquire(ctx, modelID)
		return lease, warn, err
	}

	lease, err := p.models.AcquireVariant(ctx, modelID, v)
	if err != nil {
		// Refusal here is a budget or capability answer, not a failure: fall
		// back to the model as configured and say what was dropped.
		ce := core.AsError(err)
		if ce.Code != core.CodeCapabilityUnavailable {
			return nil, warn, err
		}
		warn = append(warn, variantRefused(req, ce.Message))
		lease, err := p.models.Acquire(ctx, modelID)
		return lease, warn, err
	}
	return lease, warn, nil
}

func wantsVariant(req core.Request) bool {
	return len(req.Hotwords) > 0 || req.DecodingMethod != "" || req.MaxActivePaths > 0
}

// buildVariant turns request options into a recogniser variant, dropping the
// parts this model cannot honour and reporting each one.
func (p *Pipeline) buildVariant(req core.Request, man registry.Manifest) (asr.Variant, []core.Warning) {
	var warn []core.Warning
	v := asr.Variant{
		DecodingMethod: req.DecodingMethod,
		MaxActivePaths: req.MaxActivePaths,
	}

	if len(req.Hotwords) == 0 {
		return v, warn
	}

	if !p.opt.HotwordsEnabled {
		return v, append(warn, core.Warning{
			Code: "hotwords_unavailable",
			Message: "hotword biasing is switched off on this server " +
				"(postproc.hotwords.enabled); the words were ignored",
		})
	}

	// The decoding method the model will actually run with, which is what the
	// support check has to judge — not what the manifest alone would say.
	method := v.DecodingMethod
	if method == "" {
		method = man.Runtime.DecodingMethod
	}
	if method == "" {
		method = "greedy_search"
	}

	hasVocab := man.Files["bpe_vocab"] != ""
	if err := asr.HotwordsSupport(man.Family, man.ModelingUnit, method, hasVocab); err != nil {
		// The message, not the error string: a warning is read by a person,
		// and "capability_unavailable: " in front of it is noise they cannot
		// act on.
		return v, append(warn, core.Warning{
			Code:    "hotwords_unavailable",
			Message: core.AsError(err).Message + "; the words were ignored",
		})
	}

	buf, err := asr.Hotwords{Words: req.Hotwords}.Buffer(man.ModelingUnit)
	if err != nil {
		return v, append(warn, core.Warning{
			Code:    "hotwords_unavailable",
			Message: core.AsError(err).Message,
		})
	}
	if buf == "" {
		return v, warn
	}

	v.Hotwords = buf
	v.HotwordsScore = req.HotwordsScore
	if v.HotwordsScore == 0 {
		v.HotwordsScore = p.opt.HotwordsDefaultScore
	}
	return v, warn
}

// variantRefused names what the caller asked for and did not get. Which option
// is reported matters: a client that sent hotwords wants to know its vocabulary
// was dropped, not that "a variant" was.
func variantRefused(req core.Request, reason string) core.Warning {
	code := "decoding_method_unavailable"
	if len(req.Hotwords) > 0 {
		code = "hotwords_unavailable"
	}
	return core.Warning{
		Code:    code,
		Message: reason + "; the model's configured settings were used instead",
	}
}
