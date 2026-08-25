package asr

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Variant is the part of a recogniser's configuration that cannot be changed
// after it is loaded.
//
// sherpa-onnx settles hotwords, the decoding method and the beam width when the
// recogniser is constructed. The Go binding exposes no per-stream override —
// the C header has SherpaOnnxCreateOfflineStreamWithHotwords, the Go wrapper
// does not export it — and SetConfig mutates an instance that every concurrent
// job on that model is sharing. So the only correct way to honour these per
// request is a second resident instance, keyed by what makes it different.
//
// That is why the pool key carries a variant: two requests with different
// variants cannot share one recogniser, and pretending otherwise would either
// race or silently ignore what was asked.
type Variant struct {
	DecodingMethod string
	MaxActivePaths int
	// Hotwords is the rendered buffer, already in the model's modeling unit,
	// not the caller's word list. Rendering happens once, before the key is
	// computed, so two requests that differ only in spacing share an instance.
	Hotwords      string
	HotwordsScore float32
}

// Zero reports the variant that asks for nothing, which every ordinary request
// uses and which shares the base instance.
func (v Variant) Zero() bool {
	return v.DecodingMethod == "" && v.MaxActivePaths == 0 &&
		v.Hotwords == "" && v.HotwordsScore == 0
}

// Key identifies the variant inside a pool key. It is empty for the zero
// variant so that ordinary traffic keys on the model id alone.
//
// A hash rather than the values themselves: a hotword list can be long, and a
// pool key is a map key, a log field and part of the model id reported by the
// API. Twelve hex characters is short enough to read and far more collision
// resistance than a handful of resident models needs.
func (v Variant) Key() string {
	if v.Zero() {
		return ""
	}
	h := sha256.New()
	// Length-prefixed so that no combination of fields can be re-parsed as a
	// different combination with the same bytes.
	for _, part := range []string{
		v.DecodingMethod,
		strconv.Itoa(v.MaxActivePaths),
		v.Hotwords,
		strconv.FormatFloat(float64(v.HotwordsScore), 'g', -1, 32),
	} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// String is for logs and for the Variant field of a model listing.
func (v Variant) String() string {
	if v.Zero() {
		return ""
	}
	var parts []string
	if v.DecodingMethod != "" {
		parts = append(parts, v.DecodingMethod)
	}
	if v.MaxActivePaths > 0 {
		parts = append(parts, "paths="+strconv.Itoa(v.MaxActivePaths))
	}
	if v.Hotwords != "" {
		parts = append(parts, "hotwords="+strconv.Itoa(strings.Count(v.Hotwords, "\n")+1))
	}
	return strings.Join(parts, ",")
}
