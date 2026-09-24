package sortformer

import "math"

// realFFT is a radix-2 FFT of a real sequence, in float64, reduced to what the
// front end needs: the power spectrum of one frame.
//
// A real sequence of length n is transformed as a complex one of length n/2
// (even samples as the real part, odd as the imaginary) and the two halves are
// separated afterwards, which halves the work of a direct complex transform.
type realFFT struct {
	n       int
	half    int
	rev     []int     // bit reversal permutation of half
	cos     []float64 // twiddles of the half-length transform
	sin     []float64
	splitCo []float64 // cos(2πk/n), sin(2πk/n) for the final split
	splitSi []float64
}

func newRealFFT(n int) *realFFT {
	if n < 4 || n&(n-1) != 0 {
		panic("realFFT: size must be a power of two, at least 4")
	}
	m := n / 2
	f := &realFFT{n: n, half: m, rev: make([]int, m),
		cos: make([]float64, m/2), sin: make([]float64, m/2),
		splitCo: make([]float64, m+1), splitSi: make([]float64, m+1)}
	bits := 0
	for 1<<bits < m {
		bits++
	}
	for i := range f.rev {
		r := 0
		for b := 0; b < bits; b++ {
			r |= (i >> b & 1) << (bits - 1 - b)
		}
		f.rev[i] = r
	}
	for k := range f.cos {
		a := -2 * math.Pi * float64(k) / float64(m)
		f.cos[k], f.sin[k] = math.Cos(a), math.Sin(a)
	}
	for k := range f.splitCo {
		a := -2 * math.Pi * float64(k) / float64(n)
		f.splitCo[k], f.splitSi[k] = math.Cos(a), math.Sin(a)
	}
	return f
}

// power writes |X[k]|² for k = 0..n/2 of the real input x (len n) into out
// (len n/2+1). re and im are scratch of len n/2 each.
func (f *realFFT) power(x, out, re, im []float64) {
	m := f.half
	for i, r := range f.rev {
		re[r], im[r] = x[2*i], x[2*i+1]
	}
	for size := 2; size <= m; size <<= 1 {
		h, step := size/2, m/size
		for start := 0; start < m; start += size {
			for j := 0; j < h; j++ {
				wr, wi := f.cos[j*step], f.sin[j*step]
				a, b := start+j, start+j+h
				tr := re[b]*wr - im[b]*wi
				ti := re[b]*wi + im[b]*wr
				re[b], im[b] = re[a]-tr, im[a]-ti
				re[a], im[a] = re[a]+tr, im[a]+ti
			}
		}
	}
	// Z = FFT(z), z[k] = x[2k] + i·x[2k+1]. With E and O the spectra of the
	// even and odd samples, E[k] = (Z[k] + conj Z[m-k])/2,
	// O[k] = (Z[k] - conj Z[m-k])/2i, and X[k] = E[k] + W^k·O[k].
	for k := 0; k <= m; k++ {
		zr, zi := re[k%m], im[k%m]
		cr, ci := re[(m-k)%m], -im[(m-k)%m]
		er, ei := (zr+cr)/2, (zi+ci)/2
		// (a + ib)/2i = (b - ia)/2
		or, oi := (zi-ci)/2, -(zr-cr)/2
		wr, wi := f.splitCo[k], f.splitSi[k]
		xr := er + or*wr - oi*wi
		xi := ei + or*wi + oi*wr
		out[k] = xr*xr + xi*xi
	}
}
