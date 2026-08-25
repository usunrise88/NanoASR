package asr

import "testing"

// The zero variant has to key on the model id alone, or every ordinary request
// would look like a request for a second instance.
func TestVariantZeroHasNoKey(t *testing.T) {
	var v Variant
	if !v.Zero() {
		t.Error("the empty variant must be Zero")
	}
	if v.Key() != "" {
		t.Errorf("Key = %q, want empty for the base instance", v.Key())
	}
}

func TestVariantKeyDistinguishesConfigurations(t *testing.T) {
	base := Variant{DecodingMethod: "modified_beam_search", MaxActivePaths: 4}

	cases := []struct {
		name string
		v    Variant
	}{
		{"a different method", Variant{DecodingMethod: "greedy_search", MaxActivePaths: 4}},
		{"a different beam width", Variant{DecodingMethod: "modified_beam_search", MaxActivePaths: 8}},
		{"added hotwords", Variant{DecodingMethod: "modified_beam_search", MaxActivePaths: 4, Hotwords: "ромашка"}},
		{"a different score", Variant{DecodingMethod: "modified_beam_search", MaxActivePaths: 4, HotwordsScore: 2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.v.Key() == base.Key() {
				t.Errorf("%+v shares a key with %+v: two configurations would share one recogniser",
					c.v, base)
			}
		})
	}
}

// Length-prefixing the fields is what stops one combination from hashing to
// another. Without it, hotwords "a" with method "bc" and hotwords "ab" with
// method "c" would be the same bytes.
func TestVariantKeyIsNotAmbiguousAcrossFields(t *testing.T) {
	a := Variant{DecodingMethod: "a", Hotwords: "bc"}
	b := Variant{DecodingMethod: "ab", Hotwords: "c"}
	if a.Key() == b.Key() {
		t.Error("two different variants hash to the same key")
	}
}

func TestVariantKeyIsStable(t *testing.T) {
	v := Variant{DecodingMethod: "modified_beam_search", Hotwords: "ромашка", HotwordsScore: 1.5}
	if v.Key() != v.Key() {
		t.Error("Key is not deterministic, so a warm variant would never be reused")
	}
	if len(v.Key()) != 12 {
		t.Errorf("Key = %q, want 12 characters", v.Key())
	}
}
