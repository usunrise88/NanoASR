package coremlasr

import (
	"slices"
)

// The encoder takes one window of audio into which several speech segments
// are packed, and the layout has to keep them from seeing each other. Each
// segment starts at a feature frame divisible by the encoder's subsampling, so
// its encoder frames fall on the grid a run of it alone would give, and at
// least gap empty frames follow it — wider than any convolution reaches.
// Masks at each resolution zero what lies between segments, the attention mask
// is block-diagonal, and the rotary positions restart at every segment.

// window is the input of one encoder call and where each piece landed in it.
type window struct {
	audio  []float32 // hop*(frames+1) samples, scaled
	m0     []float32 // [frames]
	m1     []float32 // [frames after one subsampling stage]
	m2     []float32 // [encoder frames]
	amask  []float32 // [encoder frames]^2, 0 within a piece, -1e4 across
	cos    []float32 // [encoder frames][rope dim]
	sin    []float32
	places []placement
}

// placement is one piece in a window: its first encoder frame and length.
type placement struct {
	piece  int
	first  int
	frames int
}

// piece is a stretch of one segment short enough for a window.
type piece struct {
	segment int
	offset  int // first sample within the segment
	samples []float32
}

// layout is what packing needs to know about the encoder.
type layout struct {
	frames   int // feature frames per window
	gap      int // feature frames between pieces
	hop      int // samples per feature frame
	frameLen int // samples per analysis window
	scale    float32
	ropeDim  int
	cos, sin []float32 // [encoder frames][ropeDim], positions 0..
}

func subsample(t int) int { return (t-1)/2 + 1 }

// featureFrames is how many full analysis windows fit in n samples.
func (l layout) featureFrames(n int) int {
	if n < l.frameLen {
		return 0
	}
	return (n-l.frameLen)/l.hop + 1
}

func (l layout) encoderFrames(featureFrames int) int {
	return subsample(subsample(featureFrames))
}

// limit is the longest piece, in feature frames, a window can hold.
func (l layout) limit() int { return l.frames - l.gap }

// split cuts every segment a window cannot hold into equal pieces it can.
func (l layout) split(segments [][]float32) []piece {
	most := l.hop*(l.limit()-1) + l.frameLen
	var out []piece
	for i, s := range segments {
		n := max(1, (len(s)+most-1)/most)
		size := (len(s) + n - 1) / n
		for k := 0; k < n; k++ {
			lo := min(k*size, len(s))
			hi := min(lo+size, len(s))
			out = append(out, piece{segment: i, offset: lo, samples: s[lo:hi]})
		}
	}
	return out
}

// pack lays pieces into as few windows as first-fit decreasing manages. The
// order of the windows and of the pieces in them changes nothing in the
// output: pieces do not see each other.
func (l layout) pack(pieces []piece) []*window {
	order := make([]int, 0, len(pieces))
	for i, p := range pieces {
		if l.featureFrames(len(p.samples)) > 0 {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return len(pieces[b].samples) - len(pieces[a].samples)
	})

	type bin struct {
		used  int // next free feature frame, aligned
		items []int
		offs  []int
	}
	var bins []*bin
	for _, i := range order {
		f := l.featureFrames(len(pieces[i].samples))
		placed := false
		for _, b := range bins {
			if b.used+f <= l.limit() {
				b.items, b.offs = append(b.items, i), append(b.offs, b.used)
				b.used = align4(b.used + f + l.gap)
				placed = true
				break
			}
		}
		if !placed {
			bins = append(bins, &bin{used: align4(f + l.gap), items: []int{i}, offs: []int{0}})
		}
	}

	out := make([]*window, len(bins))
	for k, b := range bins {
		out[k] = l.fill(pieces, b.items, b.offs)
	}
	return out
}

func align4(n int) int { return (n + 3) / 4 * 4 }

func (l layout) fill(pieces []piece, items, offs []int) *window {
	t1 := subsample(l.frames)
	t2 := subsample(t1)
	w := &window{
		audio: make([]float32, l.hop*(l.frames+1)),
		m0:    make([]float32, l.frames),
		m1:    make([]float32, t1),
		m2:    make([]float32, t2),
		amask: make([]float32, t2*t2),
		cos:   make([]float32, t2*l.ropeDim),
		sin:   make([]float32, t2*l.ropeDim),
	}
	for i := range w.amask {
		w.amask[i] = -1e4
	}
	for k, i := range items {
		o := offs[k]
		s := pieces[i].samples
		f := l.featureFrames(len(s))
		n := min(len(s), l.hop*(f+1))
		dst := w.audio[l.hop*o:]
		for j := 0; j < n; j++ {
			dst[j] = s[j] * l.scale
		}
		fill(w.m0[o:o+f], 1)
		a1, n1 := o/2, subsample(f)
		fill(w.m1[a1:a1+n1], 1)
		a2, n2 := o/4, l.encoderFrames(f)
		fill(w.m2[a2:a2+n2], 1)
		for r := a2; r < a2+n2; r++ {
			fill(w.amask[r*t2+a2:r*t2+a2+n2], 0)
		}
		copy(w.cos[a2*l.ropeDim:(a2+n2)*l.ropeDim], l.cos[:n2*l.ropeDim])
		copy(w.sin[a2*l.ropeDim:(a2+n2)*l.ropeDim], l.sin[:n2*l.ropeDim])
		w.places = append(w.places, placement{piece: i, first: a2, frames: n2})
	}
	return w
}

func fill(s []float32, v float32) {
	for i := range s {
		s[i] = v
	}
}
