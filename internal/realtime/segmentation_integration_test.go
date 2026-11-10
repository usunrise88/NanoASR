//go:build integration

// Integration test for what happens to words at an utterance boundary that the
// engine forces, on the real Nemotron streaming model.
//
// It needs the weights unpacked somewhere, and the path in the environment:
//
//	NANOASR_NEMOTRON_DIR=/path/to/sherpa-onnx-nemotron-3.5-asr-streaming-0.6b-560ms-int8-2026-06-11 \
//	  go test -tags integration -run Segmentation -v ./internal/realtime/
//
// The package ships a Spanish clip with it. Repeated with short gaps it is one
// unbroken stretch of speech longer than the endpointer's twenty-second limit,
// and because the same sentence comes round again and again, a word that goes
// missing at a boundary shows up as a difference from the same audio decoded as
// a single utterance. Moving the clip by a fraction of a second moves where the
// limit lands in the sentence, which is what makes this a sweep rather than one
// lucky or unlucky position.
package realtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

const realModelID = "nemotron-3.5-streaming-multi"

// recogniserLength is what the server sets the recogniser's own length rule to
// (see cmd/nanoasr/wire.go): out of reach, because the engine owns the limit.
const recogniserLength = 24 * 3600

func loadNemotron(t *testing.T) (*sherpa.Online, string) {
	return loadNemotronWithRule3(t, recogniserLength)
}

func loadNemotronWithRule3(t *testing.T, rule3 float32) (*sherpa.Online, string) {
	t.Helper()
	dir := os.Getenv("NANOASR_NEMOTRON_DIR")
	if dir == "" {
		t.Skip("NANOASR_NEMOTRON_DIR is not set; point it at the unpacked nemotron-3.5 streaming model")
	}
	all, err := registry.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var man registry.Manifest
	for _, m := range all {
		if m.ID == realModelID {
			man = m
		}
	}
	if man.ID == "" {
		t.Fatalf("%s is not in the built-in catalog", realModelID)
	}
	rec, err := sherpa.NewOnline(t.Context(), man, dir, sherpa.OnlineOptions{
		NumThreads:              4,
		EnableEndpoint:          true,
		Rule1MinTrailingSilence: 2.4,
		Rule2MinTrailingSilence: 1.2,
		Rule3MinUtteranceLength: rule3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	return rec, dir
}

func readClip(t *testing.T, dir, name string) []float32 {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "test_wavs", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(t.Context(), f, audio.Options{TargetSampleRate: 16000})
	if err != nil {
		t.Fatal(err)
	}
	return pcms[0].Samples
}

func quiet(seconds float64) []float32 { return make([]float32, int(seconds*16000)) }

// speech builds lead + clip (gap clip)... + tail.
func speech(clip []float32, copies int, lead, gap, tail float64) []float32 {
	out := append([]float32(nil), quiet(lead)...)
	for i := 0; i < copies; i++ {
		if i > 0 {
			out = append(out, quiet(gap)...)
		}
		out = append(out, clip...)
	}
	return append(out, quiet(tail)...)
}

type finalItem struct {
	text       string
	start, end int64
	cut        string
	words      []core.Word
}

// transcribe feeds samples in 20 ms frames, commits at the end, and returns
// every final the engine produced.
func transcribe(t *testing.T, rec *sherpa.Online, samples []float32, auto bool, tweak func(*Options)) []finalItem {
	t.Helper()
	opt := Options{MaxSessions: 1, MaxBufferedSeconds: 10}
	if tweak != nil {
		tweak(&opt)
	}
	e := New(rec, opt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	defer func() { cancel(); <-done }()

	s, err := e.Open(ctx, core.RealtimeParams{Language: "es-ES", AutoTurns: auto})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	totalMS := int64(len(samples)) * 1000 / 16000
	var (
		mu     sync.Mutex
		finals []finalItem
	)
	last := make(chan struct{})
	go func() {
		closed := false
		for ev := range s.Events() {
			if os.Getenv("NANOASR_TRACE") != "" && ev.Kind != core.RealtimePartial {
				t.Logf("event kind=%v item=%s start=%d end=%d text=%q", ev.Kind, ev.Item, ev.StartMS, ev.EndMS, ev.Text)
			}
			if ev.Kind != core.RealtimeFinal {
				continue
			}
			mu.Lock()
			finals = append(finals, finalItem{ev.Text, ev.StartMS, ev.EndMS, ev.Cut, ev.Words})
			mu.Unlock()
			// The commit closes the item at the end of everything sent, which
			// is how its final is told from the ones before it.
			if ev.EndMS >= totalMS && !closed {
				closed = true
				close(last)
			}
		}
	}()

	const frame = 320 // 20 ms
	pace := 10 * time.Millisecond
	if v := os.Getenv("NANOASR_PACE_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil {
			pace = time.Duration(ms) * time.Millisecond
		}
	}
	for off := 0; off < len(samples); off += frame {
		end := min(off+frame, len(samples))
		if err := s.Append(16000, samples[off:end]); err != nil {
			t.Fatal(err)
		}
		// Paced, because a burst that outruns the decoder makes the engine's
		// notion of "now" in the audio timeline run ahead of what has been
		// recognised, which is not what a microphone does.
		time.Sleep(pace)
	}
	s.Commit()

	select {
	case <-last:
	case <-time.After(3 * time.Minute):
		t.Fatal("no final for the committed item")
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]finalItem(nil), finals...)
}

var nonWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

func wordsOf(texts ...string) []string {
	return strings.Fields(strings.ToLower(nonWord.ReplaceAllString(strings.Join(texts, " "), " ")))
}

// diff counts the words of want that are absent from got, and the words of got
// that want does not have, by longest common subsequence.
func diff(want, got []string) (missing, extra []string) {
	n, m := len(want), len(got)
	l := make([][]int, n+1)
	for i := range l {
		l[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if want[i] == got[j] {
				l[i][j] = l[i+1][j+1] + 1
			} else {
				l[i][j] = max(l[i+1][j], l[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case want[i] == got[j]:
			i++
			j++
		case l[i+1][j] >= l[i][j+1]:
			missing = append(missing, want[i])
			i++
		default:
			extra = append(extra, got[j])
			j++
		}
	}
	missing = append(missing, want[i:]...)
	extra = append(extra, got[j:]...)
	return
}

// uncovered returns the words of ref with no word of got near them in time.
//
// This is the measure of loss that spelling does not disturb: after a boundary
// a recogniser that starts again from nothing may write "qué" where it wrote
// "que", and a comparison of texts counts that as a lost word and a new one. A
// word that was spoken and is in nobody's transcript leaves a hole in time.
func uncovered(ref, got []core.Word, tol float64) []core.Word {
	var out []core.Word
	for _, w := range ref {
		mid := (w.Start + w.End) / 2
		found := false
		for _, g := range got {
			if m := (g.Start + g.End) / 2; m >= mid-tol && m <= mid+tol {
				found = true
				break
			}
		}
		if !found {
			out = append(out, w)
		}
	}
	return out
}

func describeWords(ws []core.Word) string {
	var parts []string
	for _, w := range ws {
		parts = append(parts, fmt.Sprintf("%s@%.1f", w.Word, w.Start))
	}
	return strings.Join(parts, " ")
}

// serverLimits is what the server's defaults give the engine.
func serverLimits(o *Options) {
	o.MaxUtterance = 20 * time.Second
	o.PauseSearch = 8 * time.Second
	o.MinPause = 400 * time.Millisecond
}

// TestSegmentationLosesNoWords decodes the same audio twice — once as one
// utterance, once with the engine's own boundaries — and reports what the
// second lacks.
//
// The two are not expected to be word for word identical: a recogniser that
// starts again from nothing at a boundary may spell a word differently, and
// that is substitution noise, not loss. What the test counts is words of the
// one-utterance transcript that the segmented one does not have, which is what
// a boundary that drops a word looks like.
func TestSegmentationLosesNoWords(t *testing.T) {
	legacy := os.Getenv("NANOASR_LEGACY") != ""
	var rec *sherpa.Online
	var dir string
	if legacy {
		// The recogniser cuts at 20 s by itself and the engine does not own the
		// limit: what the server did before.
		rec, dir = loadNemotronWithRule3(t, 20)
	} else {
		rec, dir = loadNemotron(t)
	}
	clip := readClip(t, dir, "es.wav")
	tweak := func(o *Options) {
		if !legacy {
			serverLimits(o)
		}
	}

	scenarios := []struct {
		name string
		gap  float64
	}{
		{"pauses of 0.5 s", 0.5},
		{"gaps of 0.2 s", 0.2},
		{"no gaps", 0},
	}
	if v := os.Getenv("NANOASR_SCENARIO"); v != "" {
		var keep []struct {
			name string
			gap  float64
		}
		for _, sc := range scenarios {
			if strings.Contains(sc.name, v) {
				keep = append(keep, sc)
			}
		}
		scenarios = keep
	}

	var totalMissing, totalNear, totalExtra, totalCuts int
	for _, sc := range scenarios {
		for _, lead := range leads() {
			samples := speech(clip, 6, lead, sc.gap, 1.5)

			whole := transcribe(t, rec, samples, false, nil)
			if len(whole) != 1 {
				t.Fatalf("%s, lead %.2f: the reference decode made %d utterances, want 1", sc.name, lead, len(whole))
			}
			cut := transcribe(t, rec, samples, true, tweak)

			var texts, bounds []string
			var gotWords []core.Word
			for _, it := range cut {
				texts = append(texts, it.text)
				bounds = append(bounds, fmt.Sprintf("%d-%d(%s)", it.start, it.end, it.cut))
				gotWords = append(gotWords, it.words...)
			}
			missing := uncovered(whole[0].words, gotWords, 0.35)
			extra := uncovered(gotWords, whole[0].words, 0.35)

			// What a boundary does is lose the words next to it. A word lost
			// far from any boundary is the recogniser hearing the same audio
			// differently from a different starting point, which it does.
			var near []core.Word
			for _, w := range missing {
				for _, it := range cut[:len(cut)-1] {
					if d := w.Start - float64(it.end)/1000; d > -1.2 && d < 1.2 {
						near = append(near, w)
						break
					}
				}
			}
			t.Logf("%s, lead %.2fs: %d utterances %v\n    spoken but in no transcript (%d, %d of them within 1.2 s of a boundary): %s\n    in a transcript but not spoken (%d): %s",
				sc.name, lead, len(cut), bounds, len(missing), len(near), describeWords(missing), len(extra), describeWords(extra))
			totalMissing += len(missing)
			totalNear += len(near)
			totalExtra += len(extra)
			totalCuts += len(cut) - 1
		}
	}
	t.Logf("over %d boundaries: %d words spoken and in no transcript (%d of them next to a boundary), %d in a transcript and not spoken",
		totalCuts, totalMissing, totalNear, totalExtra)

	if legacy {
		return // a measurement of what the engine did before, not a requirement
	}
	// Before the engine owned the limit, a forced boundary lost one to three
	// words, and several seconds of them when the speaker did not pause. What is
	// left is the recogniser dropping the first word of a stream it has just
	// started, which no amount of care at the boundary removes: under one word
	// in two boundaries, where it used to be one in every boundary at least.
	if totalCuts > 0 && float64(totalNear) > 0.5*float64(totalCuts) {
		t.Errorf("%d words were lost next to %d boundaries; want fewer than one in two", totalNear, totalCuts)
	}
}

// leads is the sweep of positions; NANOASR_LEADS="0,0.7" narrows it.
func leads() []float64 {
	if v := os.Getenv("NANOASR_LEADS"); v != "" {
		var out []float64
		for _, f := range strings.Split(v, ",") {
			x, err := strconv.ParseFloat(strings.TrimSpace(f), 64)
			if err == nil {
				out = append(out, x)
			}
		}
		return out
	}
	return []float64{3.0, 3.35, 3.7, 4.05, 4.4, 4.75}
}
