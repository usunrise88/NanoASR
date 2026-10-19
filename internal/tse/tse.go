// Package tse is voice isolation around the extractor: which 3 seconds of a
// recording name the voice to keep, and which stretches of the extractor's
// output are still that voice.
//
// Both answers come from a speaker embedding (CAM++), not from the extractor.
// Voxlock carries its own speaker encoder, conditioned on whatever sample it
// is given; it has no notion of whether the sample was a good one, and none of
// whether the voice it was asked for is speaking at all. Measured on two-voice
// mixtures, those are exactly where it fails: a sample taken where another
// voice bleeds in locks onto the wrong person, and while the chosen voice is
// silent the extractor tends to hand over whoever is talking instead. The
// embedding answers both questions independently of the network it checks.
package tse

import (
	"context"
	"math"
	"slices"
	"sort"

	"github.com/usunrise88/nanoasr/internal/tse/voxlock"
)

// SampleRate is the rate everything in this package works at: the
// extractor's and the embedding model's.
const SampleRate = voxlock.SampleRate

// Embedder turns speech into a speaker embedding. Implementations must be safe
// for concurrent use and return unit-length vectors.
type Embedder interface {
	Embed(samples []float32) ([]float32, error)
}

// Span is a stretch of the recording, in seconds.
type Span struct {
	Start, End float64
}

func (s Span) dur() float64 { return s.End - s.Start }

// Speaker is one voice the diarizer found: where it speaks.
type Speaker struct {
	ID    string
	Spans []Span
}

// SampleKind says how a voice sample was cut.
type SampleKind string

const (
	// SampleWindow is 3 contiguous seconds of the recording in which only
	// the chosen voice speaks.
	SampleWindow SampleKind = "window"
	// SampleAssembled is pieces of the voice's clean speech joined end to
	// end, for a voice that never speaks alone for 3 seconds at a time.
	SampleAssembled SampleKind = "assembled"
)

// Sample is the voice sample chosen for one speaker.
type Sample struct {
	Kind SampleKind
	// Start is where a window sample begins, in seconds; an assembled
	// sample has no single place in the recording and leaves it zero.
	Start float64
	// Audio is exactly voxlock.EnrollSamples long.
	Audio []float32
	// Similarity is how close the sample sounds to the speaker as a whole:
	// the cosine between its embedding and the speaker's centroid.
	Similarity float64
	// Embedding is the sample's own; the gate compares against it.
	Embedding []float32
	// CleanSpeech is how much of the speaker's speech overlaps nobody, in
	// seconds: the material the sample was chosen from.
	CleanSpeech float64
}

// Selection tunes ChooseSample.
type Selection struct {
	// MinCleanSpeech is the least speech a voice must have on its own for a
	// sample to be cut at all. Below it there is not enough of the voice to
	// be sure what it is, and the extractor would be guessing.
	MinCleanSpeech float64
	// Margin keeps a sample this far from another speaker's turn: diarizer
	// boundaries are tens of milliseconds off, and the bleed at a hand-over
	// is exactly what a sample must not contain.
	Margin float64
	// Step is the spacing of the candidate windows.
	Step float64
	// MaxCandidates and MaxCentroidSpans bound the embeddings computed, so a
	// voice that talks for an hour costs what one that talks for a minute
	// does.
	MaxCandidates    int
	MaxCentroidSpans int
}

// DefaultSelection is what the server uses.
func DefaultSelection() Selection {
	return Selection{
		MinCleanSpeech:   1.5,
		Margin:           0.15,
		Step:             0.25,
		MaxCandidates:    48,
		MaxCentroidSpans: 24,
	}
}

const sampleSeconds = float64(voxlock.EnrollSamples) / SampleRate

// ErrTooLittleSpeech reports a voice with less clean speech than a sample
// needs. It carries the amount, for the reason shown next to the voice.
type ErrTooLittleSpeech struct{ Clean float64 }

func (e ErrTooLittleSpeech) Error() string {
	return "this voice speaks on its own for too little time to sample it"
}

// ChooseSample picks the 3 seconds of audio that best stand for speaker.
//
// The speaker's centroid is the mean embedding of its own clean speech, its
// turns with everyone else's cut out. Candidates are 3-second windows that
// keep Margin away from every other speaker; the one whose embedding is
// nearest the centroid wins. A voice that never has 3 seconds to itself gets
// an assembled sample instead: its clean pieces, best first, joined and
// repeated to length.
func ChooseSample(audio []float32, speaker Speaker, others []Speaker, emb Embedder, sel Selection) (Sample, error) {
	dur := float64(len(audio)) / SampleRate
	var busy []Span
	for _, o := range others {
		if o.ID == speaker.ID {
			continue
		}
		for _, s := range o.Spans {
			busy = append(busy, Span{max(0, s.Start-sel.Margin), min(dur, s.End+sel.Margin)})
		}
	}
	busy = merge(busy)
	own := merge(clip(speaker.Spans, dur))
	clean := subtract(own, busy)
	var cleanTotal float64
	for _, s := range clean {
		cleanTotal += s.dur()
	}
	if cleanTotal < sel.MinCleanSpeech {
		return Sample{}, ErrTooLittleSpeech{Clean: cleanTotal}
	}

	centroid, err := centroidOf(audio, clean, emb, sel.MaxCentroidSpans)
	if err != nil {
		return Sample{}, err
	}

	// Windows inside the gaps between other speakers, ranked by how much of
	// them is this voice. A window of silence would be clean and useless.
	type cand struct {
		start, speech float64
	}
	var cands []cand
	for _, free := range subtract([]Span{{0, dur}}, busy) {
		for st := free.Start; st+sampleSeconds <= free.End+1e-9; st += sel.Step {
			sp := coverage(own, Span{st, st + sampleSeconds})
			if sp >= sampleSeconds/2 {
				cands = append(cands, cand{st, sp})
			}
		}
	}
	if len(cands) > 0 {
		best := 0.0
		for _, c := range cands {
			best = max(best, c.speech)
		}
		// The windows nearly as full of the voice as the fullest one, spread
		// evenly over the recording when there are too many to embed.
		keep := cands[:0]
		for _, c := range cands {
			if c.speech >= best-0.5 {
				keep = append(keep, c)
			}
		}
		keep = spread(keep, sel.MaxCandidates)

		var win Sample
		win.Similarity = math.Inf(-1)
		for _, c := range keep {
			a := slice(audio, c.start, c.start+sampleSeconds)
			e, err := emb.Embed(a)
			if err != nil {
				return Sample{}, err
			}
			if s := cosine(e, centroid); s > win.Similarity {
				win = Sample{Kind: SampleWindow, Start: c.start, Audio: fit(a), Similarity: s, Embedding: e}
			}
		}
		win.CleanSpeech = cleanTotal
		return win, nil
	}

	// No window of 3 seconds without someone else in it: assemble one from
	// the clean pieces, the ones that sound most like the voice first.
	type piece struct {
		a   []float32
		sim float64
	}
	var pieces []piece
	for _, s := range clean {
		if s.dur() < 0.3 {
			continue
		}
		a := slice(audio, s.Start, s.End)
		e, err := emb.Embed(a)
		if err != nil {
			return Sample{}, err
		}
		pieces = append(pieces, piece{a, cosine(e, centroid)})
	}
	sort.SliceStable(pieces, func(i, j int) bool { return pieces[i].sim > pieces[j].sim })
	var joined []float32
	for len(joined) < voxlock.EnrollSamples && len(pieces) > 0 {
		for _, p := range pieces {
			joined = append(joined, p.a...)
			if len(joined) >= voxlock.EnrollSamples {
				break
			}
		}
	}
	if len(joined) == 0 {
		return Sample{}, ErrTooLittleSpeech{Clean: cleanTotal}
	}
	a := fit(joined)
	e, err := emb.Embed(a)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Kind: SampleAssembled, Audio: a, Similarity: cosine(e, centroid), Embedding: e, CleanSpeech: cleanTotal}, nil
}

// SampleAt cuts a sample from a place the caller chose, for when the one
// ChooseSample picked is not the voice wanted. It is trusted as given.
func SampleAt(audio []float32, start float64, emb Embedder) (Sample, error) {
	a := fit(slice(audio, start, start+sampleSeconds))
	e, err := emb.Embed(a)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Kind: SampleWindow, Start: start, Audio: a, Similarity: 1, Embedding: e}, nil
}

// centroidOf is the mean embedding of the longest clean spans, each cut to at
// most 6 seconds so one long turn does not stand for the whole voice.
func centroidOf(audio []float32, clean []Span, emb Embedder, maxSpans int) ([]float32, error) {
	var parts []Span
	for _, s := range clean {
		for st := s.Start; st < s.End; st += 6 {
			if p := (Span{st, min(s.End, st+6)}); p.dur() >= 1 || s.dur() < 1 {
				parts = append(parts, p)
			}
		}
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].dur() > parts[j].dur() })
	if len(parts) > maxSpans {
		parts = parts[:maxSpans]
	}
	var sum []float32
	for _, p := range parts {
		if p.dur() < 0.3 {
			continue
		}
		e, err := emb.Embed(slice(audio, p.Start, p.End))
		if err != nil {
			return nil, err
		}
		if sum == nil {
			sum = make([]float32, len(e))
		}
		for i := range e {
			sum[i] += e[i] * float32(p.dur())
		}
	}
	if sum == nil {
		// Only crumbs: embed them together.
		var joined []float32
		for _, s := range clean {
			joined = append(joined, slice(audio, s.Start, s.End)...)
		}
		return emb.Embed(joined)
	}
	return normalise(sum), nil
}

// Gate mutes the stretches of an isolated recording that are not the voice it
// was isolated to.
type Gate struct {
	// Window is the audio each decision embeds; Hop is the stretch each one
	// decides. A decision needs about a second and a half of speech to be
	// reliable, which is far longer than the 250 ms it should govern.
	Window, Hop float64
	// Floor is the cosine to the voice's sample below which a stretch is
	// nobody it knows, and muted.
	Floor float64
	// Others are the samples of the other voices in the recording. A
	// stretch closer to any of them than to the chosen voice is that voice,
	// and muted.
	//
	// A fixed threshold alone does not hold: measured on two voices of one
	// speaker pitched apart, the other voice's stretches score 0.29-0.44
	// against the chosen sample and the chosen voice's own stretches
	// 0.40-0.86, so any one number cuts both. Against each other they never
	// cross: the other voice scores 0.55-0.83 against its own sample, the
	// chosen one 0.03-0.41.
	Others [][]float32
	// Fade is the crossfade at every change, so a cut is not a click.
	Fade float64
	// Silence is the RMS below which a stretch is left as it is: there is
	// nothing in it to judge, and nothing to remove.
	Silence float64
	// Keep are stretches passed whatever the embedding says: the chosen
	// speaker's own turns, where the extractor is doing what it is for and a
	// low score is more likely a short or overlapped window than a stranger.
	Keep []Span
}

// DefaultGate is what the server uses. The window was measured on two-voice
// mixtures; the floor sits under every stretch of a chosen voice in them.
func DefaultGate() Gate {
	return Gate{Window: 1.5, Hop: 0.25, Floor: 0.25, Fade: 0.1, Silence: 1e-3}
}

// Apply mutes the stretches of y that are not ref, in place, and returns it.
// progress, when set, is told how many samples have been decided; ctx is
// checked between decisions.
//
// The decisions are kept one per hop rather than one per sample: an hour of
// audio is 57.6 million samples, and a mask that size would cost as much
// memory as the audio it masks.
func (g Gate) Apply(ctx context.Context, y []float32, ref []float32, emb Embedder, progress func(done int)) ([]float32, error) {
	n := len(y)
	hop := max(1, int(g.Hop*SampleRate))
	win := int(g.Window * SampleRate)
	keep := merge(g.Keep)
	hops := make([]bool, (n+hop-1)/hop)
	for k := range hops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, end := k*hop, min(n, (k+1)*hop)
		if progress != nil {
			progress(end)
		}
		if coverage(keep, Span{float64(st) / SampleRate, float64(end) / SampleRate}) > 0 {
			hops[k] = true
			continue
		}
		c := st + hop/2
		a, b := max(0, c-win/2), min(n, c-win/2+win)
		w := y[a:b]
		if rms(w) < g.Silence {
			hops[k] = true
			continue
		}
		e, err := emb.Embed(w)
		if err != nil {
			return nil, err
		}
		hops[k] = g.owns(e, ref)
	}
	crossfade(y, hops, hop, int(g.Fade*SampleRate))
	return y, nil
}

// crossfade applies per-hop decisions to y in place, with a linear ramp of
// fade samples centred on every change so a cut is not a click. The gain at a
// sample is the share of kept samples in the fade window around it, the ends
// of the recording held.
func crossfade(y []float32, hops []bool, hop, fade int) {
	n := len(y)
	if n == 0 {
		return
	}
	half := fade / 2
	kept := func(i int) bool { return hops[min(n-1, max(0, i))/hop] }
	for k, on := range hops {
		st, end := k*hop, min(n, (k+1)*hop)
		steady := (k == 0 || hops[k-1] == on) && (k == len(hops)-1 || hops[k+1] == on)
		if steady || half == 0 {
			if !on {
				clear(y[st:end])
			}
			continue
		}
		// A sliding count over the window, so a change costs one pass over
		// its hop rather than a window's worth of work per sample.
		c := 0
		for j := st - half; j <= st+half; j++ {
			if kept(j) {
				c++
			}
		}
		for i := st; i < end; i++ {
			y[i] *= float32(c) / float32(2*half+1)
			if kept(i + half + 1) {
				c++
			}
			if kept(i - half) {
				c--
			}
		}
	}
}

// owns decides whether an embedding is the chosen voice.
func (g Gate) owns(e, ref []float32) bool {
	c := cosine(e, ref)
	if c < g.Floor {
		return false
	}
	for _, o := range g.Others {
		if cosine(e, o) > c {
			return false
		}
	}
	return true
}

func rms(s []float32) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range min(len(a), len(b)) {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func normalise(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	if n == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

// slice returns the samples between two times, clamped to the recording.
func slice(audio []float32, from, to float64) []float32 {
	a := min(len(audio), max(0, int(math.Round(from*SampleRate))))
	b := min(len(audio), max(a, int(math.Round(to*SampleRate))))
	return audio[a:b]
}

// fit makes a sample exactly as long as the extractor wants, repeating it if
// it is short.
func fit(a []float32) []float32 {
	out := make([]float32, voxlock.EnrollSamples)
	if len(a) == 0 {
		return out
	}
	for i := 0; i < len(out); i += len(a) {
		copy(out[i:], a)
	}
	return out
}

// merge sorts spans and joins the ones that touch.
func merge(spans []Span) []Span {
	s := slices.Clone(spans)
	sort.Slice(s, func(i, j int) bool { return s[i].Start < s[j].Start })
	var out []Span
	for _, x := range s {
		if x.End <= x.Start {
			continue
		}
		if n := len(out); n > 0 && x.Start <= out[n-1].End {
			out[n-1].End = max(out[n-1].End, x.End)
			continue
		}
		out = append(out, x)
	}
	return out
}

func clip(spans []Span, dur float64) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		out = append(out, Span{max(0, s.Start), min(dur, s.End)})
	}
	return out
}

// subtract returns a minus b; both must be merged.
func subtract(a, b []Span) []Span {
	var out []Span
	j := 0
	for _, s := range a {
		cur := s.Start
		for j < len(b) && b[j].End <= cur {
			j++
		}
		for k := j; k < len(b) && b[k].Start < s.End; k++ {
			if b[k].Start > cur {
				out = append(out, Span{cur, b[k].Start})
			}
			cur = max(cur, b[k].End)
		}
		if cur < s.End {
			out = append(out, Span{cur, s.End})
		}
	}
	return out
}

// coverage is how much of w the merged spans cover, in seconds.
func coverage(spans []Span, w Span) float64 {
	var c float64
	for _, s := range spans {
		if s.End <= w.Start || s.Start >= w.End {
			continue
		}
		c += min(s.End, w.End) - max(s.Start, w.Start)
	}
	return c
}

// spread keeps at most n items, evenly spaced through the slice.
func spread[T any](s []T, n int) []T {
	if n <= 0 || len(s) <= n {
		return s
	}
	out := make([]T, 0, n)
	for i := range n {
		out = append(out, s[i*len(s)/n])
	}
	return out
}
