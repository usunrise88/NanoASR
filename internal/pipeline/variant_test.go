package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/pool"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/vad"
)

// biasableRegistry describes a model hotwords can actually be applied to:
// a transducer, decoded with beam search, over a character vocabulary.
type biasableRegistry struct{ fakeRegistry }

func (r biasableRegistry) Resolve(ctx context.Context, id string) (registry.Manifest, error) {
	m, err := r.fakeRegistry.Resolve(ctx, id)
	if err != nil {
		return m, err
	}
	m.Family = "transducer"
	m.ModelingUnit = asr.UnitBPE
	// The vocabulary file is what makes a subword model biasable. No entry in
	// the catalog ships one today, which is why this is a fixture rather than a
	// description of any model we have.
	m.Files["bpe_vocab"] = "bpe.vocab"
	m.Runtime.DecodingMethod = "modified_beam_search"
	return m, nil
}

// variantHarness records the variant every load was asked for, which is the
// only way to tell a request that was honoured from one that was quietly
// dropped: both return a transcript.
type variantHarness struct {
	pipeline *Pipeline
	loaded   []asr.Variant
}

func newVariantHarness(t *testing.T, reg registry.Registry, opt variantOptions) *variantHarness {
	t.Helper()

	rec := &fakeRecognizer{
		unit: asr.UnitBPE, caps: core.Capabilities{WordTimestamps: true},
		results: []asr.Recognition{
			timedRecognition("да", []string{"да"}, []float32{0}, []float32{0.2}),
		},
	}
	h := &variantHarness{}

	models := pool.New(reg,
		func(_ context.Context, _ registry.Manifest, _ string, v asr.Variant) (asr.Recognizer, error) {
			h.loaded = append(h.loaded, v)
			return rec, nil
		},
		pool.Options{MaxResidentModels: 4, MaxModelRSSMB: 4096, MaxVariants: opt.maxVariants})
	t.Cleanup(func() { _ = models.Close() })

	if opt.DefaultModel == "" {
		opt.DefaultModel = "test-model"
	}
	h.pipeline = New(audio.NewRouter(fakeDecoder{pcm: silence(2)}),
		fakeSegmenter{segments: []vad.Segment{segment(0, 1)}},
		models, pool.NewGovernor(4), opt.Options)
	return h
}

// options carries the pool knob alongside the pipeline's, so a test can state
// both in one literal.
type variantOptions struct {
	Options
	maxVariants int
}

func TestHotwordsLoadAVariantWhenTheBudgetAllows(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true, HotwordsDefaultScore: 1.5},
		maxVariants: 1,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, Hotwords: []string{"ромашка"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(got.Warnings, "hotwords_unavailable") {
		t.Errorf("warnings %+v say the hotwords were dropped", got.Warnings)
	}
	if len(h.loaded) != 1 {
		t.Fatalf("loaded %d recognisers, want 1", len(h.loaded))
	}
	v := h.loaded[0]
	if v.Hotwords != "ромашка" {
		t.Errorf("variant hotwords = %q, want the phrase as written", v.Hotwords)
	}
	// The caller named no score, so the server's default has to fill in — the
	// OpenAI dialect maps prompt to hotwords and never sends one.
	if v.HotwordsScore != 1.5 {
		t.Errorf("HotwordsScore = %v, want the configured default 1.5", v.HotwordsScore)
	}
}

// max: 0 is the default, and it has to answer honestly rather than pretend.
func TestHotwordsWarnWhenNoVariantBudget(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true},
		maxVariants: 0,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, Hotwords: []string{"ромашка"},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := findWarning(got.Warnings, "hotwords_unavailable")
	if !ok {
		t.Fatalf("warnings %+v should say the hotwords were ignored", got.Warnings)
	}
	if !strings.Contains(w.Message, "variants.max") {
		t.Errorf("message %q should name the setting that would allow it", w.Message)
	}
	// The transcript still arrives, on the base instance.
	if len(h.loaded) != 1 || !h.loaded[0].Zero() {
		t.Errorf("loaded %+v, want one base instance", h.loaded)
	}
}

// A CTC model has no beam to bias. Answering with a transcript and no warning
// would leave the caller believing their vocabulary was applied.
func TestHotwordsWarnOnAModelThatCannotBeBiased(t *testing.T) {
	h := newVariantHarness(t, fakeRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true},
		maxVariants: 1,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, Hotwords: []string{"ромашка"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "hotwords_unavailable") {
		t.Errorf("warnings %+v should say this model cannot be biased", got.Warnings)
	}
}

// decoding_method was parsed, stored in the job record and then dropped before
// it reached the recogniser. Either it arrives or it is reported.
func TestDecodingMethodReachesTheRecogniser(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{},
		maxVariants: 1,
	})

	if _, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, DecodingMethod: "greedy_search", MaxActivePaths: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if len(h.loaded) != 1 {
		t.Fatalf("loaded %d recognisers, want 1", len(h.loaded))
	}
	if got := h.loaded[0]; got.DecodingMethod != "greedy_search" || got.MaxActivePaths != 2 {
		t.Errorf("variant = %+v, want the request's decoding settings", got)
	}
}

func TestDecodingMethodWarnsWithoutBudget(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{},
		maxVariants: 0,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, DecodingMethod: "greedy_search",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "decoding_method_unavailable") {
		t.Errorf("warnings %+v should say the decoding method was not applied", got.Warnings)
	}
}

// Hotwords switched off at the server is a different answer from no budget, and
// the message has to say which.
func TestHotwordsWarnWhenDisabledOnTheServer(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: false},
		maxVariants: 1,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, Hotwords: []string{"ромашка"},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := findWarning(got.Warnings, "hotwords_unavailable")
	if !ok {
		t.Fatalf("warnings %+v should mention the hotwords", got.Warnings)
	}
	if !strings.Contains(w.Message, "hotwords.enabled") {
		t.Errorf("message %q should name the setting that switched it off", w.Message)
	}
}

func findWarning(ws []core.Warning, code string) (core.Warning, bool) {
	for _, w := range ws {
		if w.Code == code {
			return w, true
		}
	}
	return core.Warning{}, false
}

// A CTC model asked for beam search keeps its own decoding and says so, rather
// than reaching a loader that would kill the process.
func TestDecodingMethodDroppedWhenTheFamilyCannotRunIt(t *testing.T) {
	h := newVariantHarness(t, fakeRegistry{}, variantOptions{ // fakeRegistry is nemo_ctc
		Options:     Options{},
		maxVariants: 2,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, DecodingMethod: "modified_beam_search",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(got.Warnings, "decoding_method_unavailable") {
		t.Errorf("warnings %+v should say the method was not applied", got.Warnings)
	}
	// The variant must be empty: nothing unsupported may reach the loader.
	if len(h.loaded) != 1 {
		t.Fatalf("loaded %d recognisers, want 1", len(h.loaded))
	}
	if !h.loaded[0].Zero() {
		t.Errorf("variant %+v reached the loader with an unsupported method", h.loaded[0])
	}
}

// greedyBiasable is the shape every catalog entry actually has: a model that
// could be biased, configured to decode greedily because that is what ordinary
// traffic should use.
type greedyBiasable struct{ biasableRegistry }

func (r greedyBiasable) Resolve(ctx context.Context, id string) (registry.Manifest, error) {
	m, err := r.biasableRegistry.Resolve(ctx, id)
	if err != nil {
		return m, err
	}
	m.Runtime.DecodingMethod = asr.GreedySearch
	return m, nil
}

// fakeDictionaries is the store, with only what the pipeline reads from it.
type fakeDictionaries struct {
	byKey map[string]core.Dictionary
	asked []string
}

func (f *fakeDictionaries) Get(_ context.Context, key string) (core.Dictionary, error) {
	f.asked = append(f.asked, key)
	d, ok := f.byKey[key]
	if !ok {
		return core.Dictionary{}, core.Errorf(core.CodeDictionaryNotFound, "no such dictionary: %s", key)
	}
	return d, nil
}

func (f *fakeDictionaries) List(context.Context, string, int) ([]core.Dictionary, error) {
	return nil, nil
}
func (f *fakeDictionaries) Save(_ context.Context, d core.Dictionary) (core.Dictionary, error) {
	return d, nil
}
func (f *fakeDictionaries) Delete(context.Context, string) error { return nil }

func TestADictionaryKeyBringsItsPhrasesAndItsScore(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true, HotwordsDefaultScore: 1.5},
		maxVariants: 1,
	})
	dicts := &fakeDictionaries{byKey: map[string]core.Dictionary{
		"medical": {Key: "medical", Score: 1.9, Phrases: []string{"кардиомиопатия", "амиодарон"}},
	}}
	h.pipeline.WithDictionaries(dicts)

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, HotwordDicts: []string{"medical"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(got.Warnings, "hotwords_unavailable") {
		t.Errorf("warnings %+v say the dictionary was dropped", got.Warnings)
	}
	if len(h.loaded) != 1 {
		t.Fatalf("loaded %d recognisers, want 1", len(h.loaded))
	}
	v := h.loaded[0]
	if v.Hotwords != "кардиомиопатия\nамиодарон" {
		t.Errorf("variant hotwords = %q, want the dictionary's phrases", v.Hotwords)
	}
	// The dictionary's own score outranks the server default: how hard to push
	// a list is a property of the list.
	if v.HotwordsScore != 1.9 {
		t.Errorf("HotwordsScore = %v, want the dictionary's 1.9", v.HotwordsScore)
	}
}

func TestADictionaryAndInlineHotwordsMergeWithoutRepeats(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true, HotwordsDefaultScore: 1.5},
		maxVariants: 1,
	})
	h.pipeline.WithDictionaries(&fakeDictionaries{byKey: map[string]core.Dictionary{
		"a": {Key: "a", Phrases: []string{"ромашка", "василёк"}},
	}})

	if _, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio:    &fakeSource{},
		Hotwords: []string{"василёк", "лютик"},
		// The same dictionary named twice must not produce a different
		// variant from naming it once.
		HotwordDicts: []string{"a", "a"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.loaded[0].Hotwords; got != "ромашка\nвасилёк\nлютик" {
		t.Errorf("variant hotwords = %q, want the merged list", got)
	}
}

// A key that names nothing is a typo in a parameter. Transcribing without the
// bias and mentioning it in a warning would hide it behind a result that looks
// correct.
func TestAnUnknownDictionaryIsRefused(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true},
		maxVariants: 1,
	})
	h.pipeline.WithDictionaries(&fakeDictionaries{byKey: map[string]core.Dictionary{}})

	_, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, HotwordDicts: []string{"absent"},
	})
	if err == nil {
		t.Fatal("a request naming a dictionary that does not exist was accepted")
	}
	e := core.AsError(err)
	if e.Code != core.CodeInvalidRequest || e.Param != "hotwords_dict" {
		t.Errorf("error = %+v, want invalid_request on hotwords_dict", e)
	}
	if !strings.Contains(e.Message, "absent") {
		t.Errorf("message %q should name the key", e.Message)
	}
}

// Every model in the catalog decodes greedily, and hotwords only apply during
// beam search. A request that asks for a bias and names no method gets the one
// that can carry it.
func TestHotwordsPromoteGreedyDecodingToBeamSearch(t *testing.T) {
	h := newVariantHarness(t, greedyBiasable{}, variantOptions{
		Options:     Options{HotwordsEnabled: true, HotwordsDefaultScore: 1.5},
		maxVariants: 1,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, Hotwords: []string{"ромашка"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(got.Warnings, "hotwords_unavailable") {
		t.Fatalf("warnings %+v say the hotwords were dropped", got.Warnings)
	}
	if !hasWarning(got.Warnings, "decoding_method_promoted") {
		t.Errorf("warnings %+v should say the decoding method changed", got.Warnings)
	}
	if v := h.loaded[0]; v.DecodingMethod != asr.ModifiedBeamSearch || v.Hotwords != "ромашка" {
		t.Errorf("variant = %+v, want beam search carrying the phrase", v)
	}
}

// Promotion is for a caller who said nothing about decoding. One who asked for
// greedy_search asked for something that cannot carry a bias, and is told that
// rather than overridden.
func TestAnExplicitGreedySearchIsNotOverridden(t *testing.T) {
	h := newVariantHarness(t, greedyBiasable{}, variantOptions{
		Options:     Options{HotwordsEnabled: true},
		maxVariants: 1,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, Hotwords: []string{"ромашка"},
		DecodingMethod: asr.GreedySearch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(got.Warnings, "decoding_method_promoted") {
		t.Errorf("warnings %+v overrode a decoding method the caller chose", got.Warnings)
	}
	if !hasWarning(got.Warnings, "hotwords_unavailable") {
		t.Errorf("warnings %+v should say why the bias was not applied", got.Warnings)
	}
	if v := h.loaded[0]; v.DecodingMethod != asr.GreedySearch {
		t.Errorf("variant = %+v, want the method the caller asked for", v)
	}
}

// A server with no store behind it cannot resolve a key, and says so instead of
// failing the transcription.
func TestDictionariesWithoutAStoreAreReported(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options:     Options{HotwordsEnabled: true},
		maxVariants: 1,
	})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, HotwordDicts: []string{"medical"},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := findWarning(got.Warnings, "hotwords_unavailable")
	if !ok {
		t.Fatalf("warnings %+v should say the dictionary could not be read", got.Warnings)
	}
	if !strings.Contains(w.Message, "medical") {
		t.Errorf("message %q should name the dictionary", w.Message)
	}
}

// A deployment's own vocabulary belongs to the deployment: configured once,
// applied to every request, without every client having to learn about it.
func TestStandingDictionariesApplyToEveryRequest(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options: Options{
			HotwordsEnabled:      true,
			HotwordsDefaultScore: 1.5,
			HotwordDictionaries:  []string{"house"},
		},
		maxVariants: 1,
	})
	h.pipeline.WithDictionaries(&fakeDictionaries{byKey: map[string]core.Dictionary{
		"house": {Key: "house", Score: 1.7, Phrases: []string{"ромашка"}},
	}})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) > 0 && hasWarning(got.Warnings, "hotwords_unavailable") {
		t.Errorf("warnings %+v say the standing dictionary was dropped", got.Warnings)
	}
	if len(h.loaded) != 1 {
		t.Fatalf("loaded %d recognisers, want 1", len(h.loaded))
	}
	if v := h.loaded[0]; v.Hotwords != "ромашка" || v.HotwordsScore != 1.7 {
		t.Errorf("variant = %+v, want the configured dictionary applied with its own score", v)
	}
}

// What the caller adds is an addition to the house vocabulary, not a
// replacement for it — and the caller's own dictionary decides the score,
// because it is the more specific of the two.
func TestARequestAddsToTheStandingDictionaries(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options: Options{
			HotwordsEnabled:     true,
			HotwordDictionaries: []string{"house"},
		},
		maxVariants: 1,
	})
	h.pipeline.WithDictionaries(&fakeDictionaries{byKey: map[string]core.Dictionary{
		"house": {Key: "house", Score: 1.7, Phrases: []string{"ромашка"}},
		"mine":  {Key: "mine", Score: 2.0, Phrases: []string{"василёк"}},
	}})

	if _, err := h.pipeline.Transcribe(context.Background(), core.Request{
		Audio: &fakeSource{}, HotwordDicts: []string{"mine"}, Hotwords: []string{"лютик"},
	}); err != nil {
		t.Fatal(err)
	}
	v := h.loaded[0]
	if v.Hotwords != "ромашка\nвасилёк\nлютик" {
		t.Errorf("variant hotwords = %q, want the standing list first", v.Hotwords)
	}
	if v.HotwordsScore != 2.0 {
		t.Errorf("HotwordsScore = %v, want the request's own dictionary to decide", v.HotwordsScore)
	}
}

// A server-wide setting must not put a warning on every request to a model it
// was never meant for. The streaming model behind the realtime dialect is never
// biasable, and neither is every CTC model in the catalog.
func TestStandingDictionariesAreSkippedOnAModelThatCannotBeBiased(t *testing.T) {
	h := newVariantHarness(t, fakeRegistry{}, variantOptions{
		Options: Options{
			HotwordsEnabled:     true,
			HotwordDictionaries: []string{"house"},
		},
		maxVariants: 1,
	})
	dicts := &fakeDictionaries{byKey: map[string]core.Dictionary{
		"house": {Key: "house", Phrases: []string{"ромашка"}},
	}}
	h.pipeline.WithDictionaries(dicts)

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings %+v, want none: nobody asked for a bias on this model", got.Warnings)
	}
	if len(dicts.asked) != 0 {
		t.Errorf("the store was read for %v on a model that cannot be biased", dicts.asked)
	}
	if len(h.loaded) != 1 || !h.loaded[0].Zero() {
		t.Errorf("loaded %+v, want the base instance", h.loaded)
	}
}

// Deleting a dictionary that the configuration still names must not take every
// request on the server down with it. The caller's own key is the opposite case
// and is still refused, which TestAnUnknownDictionaryIsRefused covers.
func TestADeletedStandingDictionaryIsReportedNotFatal(t *testing.T) {
	h := newVariantHarness(t, biasableRegistry{}, variantOptions{
		Options: Options{
			HotwordsEnabled:     true,
			HotwordDictionaries: []string{"gone", "house"},
		},
		maxVariants: 1,
	})
	h.pipeline.WithDictionaries(&fakeDictionaries{byKey: map[string]core.Dictionary{
		"house": {Key: "house", Phrases: []string{"ромашка"}},
	}})

	got, err := h.pipeline.Transcribe(context.Background(), core.Request{Audio: &fakeSource{}})
	if err != nil {
		t.Fatalf("a deleted standing dictionary failed the request: %v", err)
	}
	w, ok := findWarning(got.Warnings, "hotwords_dict_missing")
	if !ok {
		t.Fatalf("warnings %+v should name the dictionary that is gone", got.Warnings)
	}
	if !strings.Contains(w.Message, "gone") || !strings.Contains(w.Message, "default_dictionaries") {
		t.Errorf("message %q should name the key and the setting", w.Message)
	}
	// The ones that are still there are still applied.
	if v := h.loaded[0]; v.Hotwords != "ромашка" {
		t.Errorf("variant hotwords = %q, want the surviving dictionary", v.Hotwords)
	}
}
