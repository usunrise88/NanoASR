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
	// The manifest decides whether a request is even expressible, and it is
	// readable without loading anything. Read once: the standing dictionaries
	// need it to know whether this model can use them, and the variant needs
	// it a few lines later for the same reasons.
	//
	// A failure is not reported here. Acquire below fails on the same model
	// with a better message, and a request that named no dictionary and wanted
	// no variant should not start failing because a manifest lookup moved.
	man, manErr := p.models.Manifest(ctx, modelID)

	req, standing := p.withStandingDictionaries(man, manErr, req)

	req, dictWarn, err := p.applyDictionaries(ctx, req, standing)
	if err != nil {
		return nil, nil, err
	}

	if !wantsVariant(req) {
		lease, err := p.models.Acquire(ctx, modelID)
		return lease, dictWarn, err
	}
	if manErr != nil {
		return nil, nil, manErr
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

// withStandingDictionaries adds the server's own dictionaries to the ones the
// request named, for a model that can actually use them. It takes the manifest
// its caller already read rather than reading it again.
//
// The capability check is the whole point of doing this here rather than in the
// parser. A standing list is configured once for a deployment whose models may
// not all be biasable — the streaming model behind the realtime dialect never
// is — and a server-wide setting must not put a warning on every request to a
// model it was never meant for. A dictionary the caller named is different:
// that one is answered, because somebody asked for it.
//
// They go in front of the request's own keys, so what a caller adds is read as
// an addition to the house vocabulary rather than a replacement for it.
//
// It returns how many keys it put in front, because a configured dictionary
// that has gone missing is reported while one the caller named is refused, and
// nothing downstream can otherwise tell the two apart. The count is a return
// value rather than a field on core.Request: it is the pipeline talking to
// itself, and a field would be one a dialect could set.
func (p *Pipeline) withStandingDictionaries(man registry.Manifest, manErr error, req core.Request) (core.Request, int) {
	if len(p.opt.HotwordDictionaries) == 0 || manErr != nil {
		return req, 0
	}
	if ok, _ := asr.HotwordsCapability(man.Family, man.ModelingUnit, man.Files["bpe_vocab"] != ""); !ok {
		return req, 0
	}
	req.HotwordDicts = append(append([]string{}, p.opt.HotwordDictionaries...), req.HotwordDicts...)
	return req, len(p.opt.HotwordDictionaries)
}

// applyDictionaries turns the dictionary keys a request named into phrases on
// the request itself, so everything downstream sees one list and does not have
// to know where it came from.
//
// A key the caller named and that names nothing is a client error rather than a
// warning. The alternative — transcribing without the bias and mentioning it in
// passing — hides a typo in a parameter behind a result that looks fine, and
// the usual case for naming a dictionary is that somebody depends on it being
// applied. A key from the configuration is the other way round: it is not this
// caller's mistake, and failing every request on the server because an operator
// deleted a dictionary would be a poor trade for a bias nobody asked for. That
// one is reported and skipped.
//
// The phrases are read now, not when the job was queued: a dictionary edited
// while a job waits is applied as it stands when the job runs, which is the
// behaviour somebody fixing a wrong phrase expects.
func (p *Pipeline) applyDictionaries(ctx context.Context, req core.Request, standingCount int) (core.Request, []core.Warning, error) {
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

	var warn []core.Warning
	seen := map[string]bool{}
	// Two lists rather than one: a dictionary the caller named outranks a
	// standing one when the score is being decided, however the phrases are
	// ordered.
	var standing, asked []core.Dictionary

	for i, key := range req.HotwordDicts {
		fromConfig := i < standingCount
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true

		d, err := p.dictionaries.Get(ctx, key)
		if err != nil {
			if core.AsError(err).Code != core.CodeDictionaryNotFound {
				return req, nil, err
			}
			if fromConfig {
				warn = append(warn, core.Warning{
					Code: "hotwords_dict_missing",
					Message: "the configured dictionary " + key + " no longer exists " +
						"(postproc.hotwords.default_dictionaries); the rest were applied",
				})
				continue
			}
			return req, nil, core.Errorf(core.CodeInvalidRequest,
				"no hotword dictionary with the key %q", key).WithParam("hotwords_dict")
		}
		if fromConfig {
			standing = append(standing, d)
		} else {
			asked = append(asked, d)
		}
	}

	req.Hotwords = hotwords.Merge(append(standing, asked...), req.Hotwords)
	// A dictionary carries the strength its author chose; an explicit score on
	// the request outranks it, and a dictionary the caller named outranks one
	// the configuration supplied. With several at the same rank the first one
	// that states a preference wins — picking the largest would let one
	// aggressive list speak for the rest.
	if req.HotwordsScore == 0 {
		for _, d := range append(asked, standing...) {
			if d.Score > 0 {
				req.HotwordsScore = d.Score
				break
			}
		}
	}
	return req, warn, nil
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
	//
	// It is decided here and applied after the support check, because a
	// promotion for a bias that is then dropped is worse than no promotion: it
	// loads a second resident copy of the model to run a slower search for
	// nothing, and tells the caller both that the method changed and that the
	// words were ignored.
	promote := method != asr.ModifiedBeamSearch && req.DecodingMethod == "" &&
		asr.DecodingSupport(man.Family, asr.ModifiedBeamSearch) == nil
	if promote {
		method = asr.ModifiedBeamSearch
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

	if promote {
		v.DecodingMethod = method
		warn = append(warn, core.Warning{
			Code: "decoding_method_promoted",
			Message: "hotwords apply during beam search, so this request decoded with " +
				"modified_beam_search instead of the model's configured greedy_search",
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
