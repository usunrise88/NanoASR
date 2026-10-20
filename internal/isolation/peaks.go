package isolation

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/usunrise88/nanoasr/internal/core"
)

// maxBuckets bounds a waveform: more columns than a screen has pixels are
// work for nothing.
const maxBuckets = 8000

func (s *Service) Peaks(ctx context.Context, id, speaker string, buckets int) (*core.AudioPeaks, error) {
	if buckets <= 0 || buckets > maxBuckets {
		return nil, core.Errorf(core.CodeInvalidRequest, "buckets must be between 1 and %d", maxBuckets).
			WithParam("buckets")
	}
	s.mu.Lock()
	sess, err := s.getLocked(ctx, id)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var path string
	switch {
	case speaker != "":
		t, err := s.pickTrackLocked(sess, speaker)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		path = filepath.Join(sess.dir, trackFile(t.Speaker))
	case sess.view.Duration > 0:
		path = filepath.Join(sess.dir, sourceFile)
	default:
		s.mu.Unlock()
		return nil, core.Errorf(core.CodeIsolationBusy, "isolation %s has not decoded its recording yet", id)
	}
	s.mu.Unlock()
	return peaksOf(path, buckets)
}

// peaksOf reads 16-bit PCM once, a block at a time, into an RMS envelope —
// the same envelope the web player draws from a file it has decoded itself.
func peaksOf(path string, buckets int) (*core.AudioPeaks, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, core.Errorf(core.CodeIsolationBusy, "the audio is gone").WithCause(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, core.Errorf(core.CodeInternal, "cannot read the audio").WithCause(err)
	}
	n := info.Size() / 2
	out := &core.AudioPeaks{Duration: round2(float64(n) / SampleRate), Values: make([]float32, buckets)}
	if n == 0 {
		return out, nil
	}
	sums := make([]float64, buckets)
	counts := make([]int64, buckets)
	buf := make([]byte, 1<<16)
	var at int64
	for {
		k, err := io.ReadFull(f, buf)
		for i := 0; i+1 < k; i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(buf[i:]))) / 32768
			b := int(at * int64(buckets) / n)
			sums[b] += v * v
			counts[b]++
			at++
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, core.Errorf(core.CodeInternal, "cannot read the audio").WithCause(err)
		}
	}
	var peak float64
	rms := make([]float64, buckets)
	for i := range rms {
		if counts[i] > 0 {
			rms[i] = math.Sqrt(sums[i] / float64(counts[i]))
			peak = max(peak, rms[i])
		}
	}
	for i, v := range rms {
		if peak > 0 {
			v /= peak
		}
		// Three places are more than a column of pixels can show, and a third
		// of the JSON.
		out.Values[i] = float32(math.Round(v*1000) / 1000)
	}
	return out, nil
}
