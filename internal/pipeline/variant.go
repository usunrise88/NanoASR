package pipeline

import (
	"context"
	"strings"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/hotwords"
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
	req, dictWarn, err := p.applyDictionaries(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	if !wantsVariant(req) {
		lease, err := p.models.Acquire(ctx, modelID)
		return lease, dictWarn, err
	}

	// The manifest decides whether the request is even expressible, and it is
	// readable without loading anything.
	man, err := p.models.Manifest(ctx, modelID)
	if err != nil {
		return nil, nil, err
	}

	v, warn := p.buildVariant(req, man)
	warn = append(dictWarn, warn...)
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

// applyDictionaries turns the dictionary keys a request named into phrases on
// the request itself, so everything downstream sees one list and does not have
// to know where it came from.
//
// A key that names nothing is a client error rather than a warning. The
// alternative — transcribing without the bias and mentioning it in passing —
// hides a typo in a parameter behind a result that looks fine, and the usual
// case for naming a dictionary is that somebody depends on it being applied.
//
// The phrases are read now, not when the job was queued: a dictionary edited
// while a job waits is applied as it stands when the job runs, which is the
// behaviour somebody fixing a wrong phrase expects.
func (p *Pipeline) applyDictionaries(ctx context.Context, req core.Request) (core.Request, []core.Warning, error) {
	if len(req.HotwordDicts) == 0 {
		return req, nil, nil
	}
	if p.dictionaries == nil {
		return req, []core.Warning{{
			Code: "hotwords_unavailable",
			Message: "this server keeps no hotword dictionaries, so " +
				strings.Join(req.HotwordDicts, ", ") + " could not be applied",
		}}, nil
	}

	seen := map[string]bool{}
	dicts := make([]core.Dictionary, 0, len(req.HotwordDicts))
	for _, key := range req.HotwordDicts {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		d, err := p.dictionaries.Get(ctx, key)
		if err != nil {
			if core.AsError(err).Code == core.CodeDictionaryNotFound {
				return req, nil, core.Errorf(core.CodeInvalidRequest,
					"no hotword dictionary with the key %q", key).WithParam("hotwords_dict")
			}
			return req, nil, err
		}
		dicts = append(dicts, d)
	}

	req.Hotwords = hotwords.Merge(dicts, req.Hotwords)
	// A dictionary carries the strength its author chose; an explicit score on
	// the request outranks it. With several dictionaries and no score on the
	// request, the first one that states a preference wins — picking the
	// largest would let one aggressive list speak for the rest.
	if req.HotwordsScore == 0 {
		for _, d := range dicts {
			if d.Score > 0 {
				req.HotwordsScore = d.Score
				break
			}
		}
	}
	return req, nil, nil
}

// buildVariant turns request options into a recogniser variant, dropping the
// parts this model cannot honour and reporting each one.
func (p *Pipeline) buildVariant(req core.Request, man registry.Manifest) (asr.Variant, []core.Warning) {
	var warn []core.Warning
	v := asr.Variant{
		DecodingMethod: req.DecodingMethod,
		MaxActivePaths: req.MaxActivePaths,
	}

	// A method this family cannot run is dropped here rather than refused,
	// because the model still has a perfectly good answer with its own
	// settings. It has to be dropped before the loader sees it: sherpa-onnx
	// terminates the process rather than declining.
	if err := asr.DecodingSupport(man.Family, v.DecodingMethod); err != nil {
		warn = append(warn, core.Warning{
			Code:    "decoding_method_unavailable",
			Message: core.AsError(err).Message + "; the model's configured method was used",
		})
		v.DecodingMethod = ""
		v.MaxActivePaths = 0
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
		method = asr.GreedySearch
	}

	// Biasing happens during beam search, and every model in the catalog is
	// configured to decode greedily. Leaving the caller to work that out meant
	// hotwords sent on their own were dropped with a warning on a model that
	// could have honoured them — and the OpenAI dialect, which maps prompt to
	// hotwords, has no way to ask for a decoding method at all.
	//
	// So a request that asks for hotwords and does not state a method gets the
	// one that can serve it, and is told. A caller who did name greedy_search
	// is left alone: they asked for something that cannot carry a bias, and
	// the refusal below says so rather than overriding them.
	if method != asr.ModifiedBeamSearch && req.DecodingMethod == "" &&
		asr.DecodingSupport(man.Family, asr.ModifiedBeamSearch) == nil {
		method = asr.ModifiedBeamSearch
		v.DecodingMethod = method
		warn = append(warn, core.Warning{
			Code: "decoding_method_promoted",
			Message: "hotwords apply during beam search, so this request decoded with " +
				"modified_beam_search instead of the model's configured greedy_search",
		})
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
