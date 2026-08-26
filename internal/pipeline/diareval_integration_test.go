//go:build integration

package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/diarize"
	diarizesherpa "github.com/usunrise88/nanoasr/internal/diarize/sherpa"
)

// Diarization error rate against a reference, on Russian dialogue.
//
// This exists because diarization was the one stage with no number attached to
// it. The fixture it had was a single voice pitch-shifted, which cannot measure
// speaker separation at all — there is one speaker in it — so every judgement
// about embedding models and thresholds was a guess dressed as a measurement.
//
// The reference comes from scripts/fetch-diar-eval.sh: real dialogue between
// two people, cut into utterances by the corpus and concatenated back with
// known boundaries. Nothing here is estimated.
//
// It asserts almost nothing, like TestM1Report: DER depends on the recording,
// and a threshold would pass on one dialog and fail on the next while telling
// nobody anything. What it does is print a table you can read while tuning.

// derCollar is the tolerance around a reference boundary, in seconds. Boundary
// frames are excluded from scoring because a reference cut by hand — or, here,
// by a corpus — is not accurate to the millisecond, and a system should not be
// charged for the disagreement. 0.25 is the convention.
const derCollar = 0.25

// derFrame is the scoring resolution. Ten milliseconds is finer than any
// boundary either side can defend and makes the arithmetic a matter of counting.
const derFrame = 0.01

type refTurn struct {
	start, end float64
	speaker    string
}

// derResult is the standard decomposition: a system can be wrong by missing
// speech, by inventing it, or by attributing it to the wrong person, and the
// three have entirely different causes.
type derResult struct {
	miss, falseAlarm, confusion float64
	refSpeech                   float64
	hypSpeakers, refSpeakers    int
	scoredFrames                int
}

func (d derResult) rate() float64 {
	if d.refSpeech == 0 {
		return 0
	}
	return (d.miss + d.falseAlarm + d.confusion) / d.refSpeech
}

func TestDiarizationErrorRate(t *testing.T) {
	root := repoRoot(t)
	base := filepath.Join(root, "testdata", "audio", "diar-masha_dima_part1-115")
	wavPath, rttmPath := base+".wav", base+".rttm"
	if _, err := os.Stat(wavPath); err != nil {
		t.Skipf("no diarization fixture; run ./scripts/fetch-diar-eval.sh (%v)", err)
	}

	ref := readRTTM(t, rttmPath)
	pcm := decodeFixture(t, wavPath)

	t.Logf("fixture %s: %.1f min, %d reference turns, %d speakers",
		filepath.Base(wavPath), pcm.Duration()/60, len(ref), countSpeakers(ref))

	// The grid is the question this test was written to answer: which of these
	// choices is doing the work, and which was superstition. Narrow it with
	// DIAR_EMBEDDINGS and DIAR_THRESHOLDS while chasing one of them — a full
	// sweep is twenty minutes, and a single row is forty seconds.
	embeddings := envList("DIAR_EMBEDDINGS",
		[]string{"campplus-sv-voxceleb", "campplus-sv-zh-en", "wespeaker-voxceleb-resnet34"})
	thresholds := envFloats("DIAR_THRESHOLDS", []float32{0.40, 0.50, 0.60, 0.70, 0.80})
	clusters := []int{0, 2}

	t.Logf("%-30s %6s %8s %6s %7s %7s %7s %7s",
		"embedding", "thresh", "clusters", "found", "DER", "miss", "falarm", "conf")

	for _, emb := range embeddings {
		for _, threshold := range thresholds {
			for _, k := range clusters {
				// A fixed cluster count ignores the threshold, so scoring the
				// same run four times would only pad the table.
				if k > 0 && threshold != thresholds[0] {
					continue
				}
				name := fmt.Sprintf("%s/thr=%.2f/k=%d", emb, threshold, k)
				t.Run(name, func(t *testing.T) {
					d := newDiarizerFor(t, emb, threshold)
					turns, err := d.Process(context.Background(), pcm, k)
					if err != nil {
						t.Fatalf("diarize: %v", err)
					}
					got := scoreDER(ref, turns)

					label := fmt.Sprintf("%.2f", threshold)
					if k > 0 {
						label = "—"
					}
					t.Logf("%-30s %6s %8d %6d %6.1f%% %6.1f%% %6.1f%% %6.1f%%",
						emb, label, k, got.hypSpeakers, 100*got.rate(),
						100*got.miss/got.refSpeech, 100*got.falseAlarm/got.refSpeech,
						100*got.confusion/got.refSpeech)
				})
			}
		}
	}
}

// envList overrides a grid axis from the environment, comma separated.
func envList(key string, fallback []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envFloats(key string, fallback []float32) []float32 {
	var out []float32
	for _, s := range envList(key, nil) {
		f, err := strconv.ParseFloat(s, 32)
		if err != nil {
			continue
		}
		out = append(out, float32(f))
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func newDiarizerFor(t *testing.T, embedding string, threshold float32) *diarizesherpa.Pool {
	t.Helper()
	reg := newRegistryFor(t)

	resolve := func(id string) string {
		t.Helper()
		man, err := reg.Resolve(t.Context(), id)
		if err != nil {
			t.Skipf("model %s is absent; run ./scripts/fetch-dev-models.sh (%v)", id, err)
		}
		dir, err := reg.Dir(id)
		if err != nil {
			t.Fatal(err)
		}
		path, err := man.FilePath(dir, "model")
		if err != nil {
			t.Fatal(err)
		}
		return path
	}

	d, err := diarizesherpa.NewPool(diarize.Config{
		SegmentationModel: resolve("pyannote-segmentation-3"),
		EmbeddingModel:    resolve(embedding),
		Threshold:         threshold,
		MinDurationOn:     0.3,
		MinDurationOff:    0.5,
	}, 4, 1)
	if err != nil {
		t.Fatalf("building the diarizer: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func decodeFixture(t *testing.T, path string) audio.PCM {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tracks, err := audio.NewWAVDecoder().Decode(t.Context(), f, audio.Options{
		TargetSampleRate: 16000,
		MaxDurationSec:   4 * 60 * 60,
	})
	if err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	return tracks[0]
}

// readRTTM parses the reference. Only SPEAKER lines matter, and only three of
// their fields: start, duration and the label.
func readRTTM(t *testing.T, path string) []refTurn {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []refTurn
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[0] != "SPEAKER" {
			continue
		}
		start, err1 := strconv.ParseFloat(fields[3], 64)
		dur, err2 := strconv.ParseFloat(fields[4], 64)
		if err1 != nil || err2 != nil {
			t.Fatalf("malformed RTTM line: %q", sc.Text())
		}
		out = append(out, refTurn{start: start, end: start + dur, speaker: fields[7]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("no SPEAKER lines in %s", path)
	}
	return out
}

// scoreDER frames both timelines and counts disagreement.
//
// Frame counting rather than interval arithmetic: the two are equivalent at
// this resolution, and one of them is obviously correct on first reading.
// Speakers are matched by the assignment that minimises confusion, because the
// labels a diarizer invents carry no meaning — spk_0 is whoever it heard first.
func scoreDER(ref []refTurn, hyp []diarize.Turn) derResult {
	end := 0.0
	for _, r := range ref {
		end = max(end, r.end)
	}
	for _, h := range hyp {
		end = max(end, h.End)
	}
	n := int(end/derFrame) + 1

	refAt := make([]string, n)
	hypAt := make([]string, n)
	scored := make([]bool, n)
	for i := range scored {
		scored[i] = true
	}

	for _, r := range ref {
		for i := frameOf(r.start); i < frameOf(r.end) && i < n; i++ {
			refAt[i] = r.speaker
		}
		// The collar straddles both edges of every reference turn.
		for _, edge := range []float64{r.start, r.end} {
			lo, hi := frameOf(edge-derCollar), frameOf(edge+derCollar)
			for i := max(lo, 0); i < hi && i < n; i++ {
				scored[i] = false
			}
		}
	}
	for _, h := range hyp {
		for i := frameOf(h.Start); i < frameOf(h.End) && i < n; i++ {
			hypAt[i] = "spk_" + strconv.Itoa(h.Speaker)
		}
	}

	mapping := bestMapping(refAt, hypAt, scored)

	var out derResult
	out.refSpeakers = countSpeakers(ref)
	seen := map[string]bool{}
	for _, h := range hypAt {
		if h != "" {
			seen[h] = true
		}
	}
	out.hypSpeakers = len(seen)

	for i := range n {
		if !scored[i] {
			continue
		}
		out.scoredFrames++
		r, h := refAt[i], hypAt[i]
		if r != "" {
			out.refSpeech += derFrame
		}
		switch {
		case r == "" && h == "":
		case r == "" && h != "":
			out.falseAlarm += derFrame
		case r != "" && h == "":
			out.miss += derFrame
		case mapping[r] != h:
			out.confusion += derFrame
		}
	}
	return out
}

// bestMapping pairs reference speakers with hypothesis speakers so that the
// most speech lines up. Exhaustive over injective maps: a conversation this
// server is for has a handful of speakers, and the exact answer is cheaper to
// write than an approximate one is to justify.
func bestMapping(refAt, hypAt []string, scored []bool) map[string]string {
	refSpk := distinct(refAt)
	hypSpk := distinct(hypAt)

	overlap := map[string]map[string]int{}
	for _, r := range refSpk {
		overlap[r] = map[string]int{}
	}
	for i := range refAt {
		if scored[i] && refAt[i] != "" && hypAt[i] != "" {
			overlap[refAt[i]][hypAt[i]]++
		}
	}

	best := map[string]string{}
	bestScore := -1
	var walk func(idx int, used map[string]bool, cur map[string]string, score int)
	walk = func(idx int, used map[string]bool, cur map[string]string, score int) {
		if idx == len(refSpk) {
			if score > bestScore {
				bestScore = score
				best = map[string]string{}
				for k, v := range cur {
					best[k] = v
				}
			}
			return
		}
		r := refSpk[idx]
		// Leaving a reference speaker unmatched is legal: a system that found
		// one speaker cannot be paired with two.
		walk(idx+1, used, cur, score)
		for _, h := range hypSpk {
			if used[h] {
				continue
			}
			used[h] = true
			cur[r] = h
			walk(idx+1, used, cur, score+overlap[r][h])
			delete(cur, r)
			delete(used, h)
		}
	}
	walk(0, map[string]bool{}, map[string]string{}, 0)
	return best
}

func distinct(labels []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range labels {
		if l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func countSpeakers(ref []refTurn) int {
	seen := map[string]bool{}
	for _, r := range ref {
		seen[r.speaker] = true
	}
	return len(seen)
}

func frameOf(sec float64) int {
	if sec < 0 {
		return 0
	}
	return int(sec / derFrame)
}
