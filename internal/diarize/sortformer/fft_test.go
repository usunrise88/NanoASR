package sortformer

import (
	"math"
	"testing"
)

func TestRealFFTMatchesDFT(t *testing.T) {
	for _, n := range []int{4, 8, 64, 512} {
		f := newRealFFT(n)
		x := make([]float64, n)
		for i := range x {
			x[i] = unit(uint64(n*1000+i)) - 0.5
		}
		got := make([]float64, n/2+1)
		f.power(x, got, make([]float64, n/2), make([]float64, n/2))
		for k := 0; k <= n/2; k++ {
			var re, im float64
			for j, v := range x {
				a := -2 * math.Pi * float64(j*k) / float64(n)
				re += v * math.Cos(a)
				im += v * math.Sin(a)
			}
			want := re*re + im*im
			if d := math.Abs(got[k] - want); d > 1e-9*math.Max(1, want) {
				t.Fatalf("n=%d bin %d: %g, DFT %g", n, k, got[k], want)
			}
		}
	}
}
