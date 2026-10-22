package coremlasr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

func TestPackKeepsPiecesApart(t *testing.T) {
	l := layout{frames: 2048, gap: 8, hop: 160, frameLen: 320, scale: 1, ropeDim: 2,
		cos: make([]float32, 512*2), sin: make([]float32, 512*2)}
	segs := [][]float32{make([]float32, 16000*3+17), make([]float32, 16000*7), make([]float32, 16000*25), make([]float32, 100)}
	pieces := l.split(segs)
	if len(pieces) != 5 {
		t.Fatalf("got %d pieces, want the 25 s segment in two and the rest whole", len(pieces))
	}
	windows := l.pack(pieces)
	seen := map[int]bool{}
	for _, w := range windows {
		used := make([]bool, len(w.m2))
		for _, p := range w.places {
			if seen[p.piece] {
				t.Errorf("piece %d packed twice", p.piece)
			}
			seen[p.piece] = true
			if p.frames != l.encoderFrames(l.featureFrames(len(pieces[p.piece].samples))) {
				t.Errorf("piece %d: %d frames", p.piece, p.frames)
			}
			for f := p.first; f < p.first+p.frames; f++ {
				if used[f] {
					t.Fatalf("pieces overlap at encoder frame %d", f)
				}
				used[f] = true
			}
			// A gap of at least two encoder frames follows every piece.
			for f := p.first + p.frames; f < min(p.first+p.frames+2, len(used)); f++ {
				if w.m2[f] != 0 {
					t.Errorf("no gap after piece %d", p.piece)
				}
			}
		}
	}
	if len(seen) != 4 { // the 100-sample segment has no frame
		t.Errorf("packed %d pieces, want 4", len(seen))
	}
}

// TestDecodeMatchesReference decodes the segments of a NanoASR result with the
// CoreML model and compares them with reference transcripts of the same
// segments. Needs NANOASR_COREML_GIGAAM (a model directory),
// NANOASR_GIGAAM_WAV (16 kHz mono), NANOASR_GIGAAM_SEGMENTS (a job result
// whose segments to cut) and NANOASR_GIGAAM_REFS (a JSON list of transcripts).
func TestDecodeMatchesReference(t *testing.T) {
	dir, wav := os.Getenv("NANOASR_COREML_GIGAAM"), os.Getenv("NANOASR_GIGAAM_WAV")
	segPath, refPath := os.Getenv("NANOASR_GIGAAM_SEGMENTS"), os.Getenv("NANOASR_GIGAAM_REFS")
	if dir == "" || wav == "" || segPath == "" || refPath == "" {
		t.Skip("the CoreML GigaAM test inputs are not set")
	}
	f, err := os.Open(wav)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(context.Background(), f,
		audio.Options{TargetSampleRate: 16000, ChannelMode: core.ChannelDownmix})
	if err != nil {
		t.Fatal(err)
	}
	x := pcms[0].Samples
	var job struct {
		Result struct {
			Segments []struct{ Start, End float64 } `json:"segments"`
		} `json:"result"`
	}
	b, _ := os.ReadFile(segPath)
	if err := json.Unmarshal(b, &job); err != nil {
		t.Fatal(err)
	}
	var refs []string
	b, _ = os.ReadFile(refPath)
	if err := json.Unmarshal(b, &refs); err != nil {
		t.Fatal(err)
	}
	var batch [][]float32
	speech := 0.0
	for _, s := range job.Result.Segments {
		batch = append(batch, x[int(s.Start*16000):int(s.End*16000)])
		speech += s.End - s.Start
	}

	var cfg gigaConfig
	b, _ = os.ReadFile(filepath.Join(dir, "config.json"))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	m := registry.Manifest{ID: "gigaam-coreml", Family: FamilyGigaAMCTC, ModelingUnit: "bpe", Files: map[string]string{
		"encoder": "encoder.mlmodelc", "rope": "rope.bin", "tokens": "tokens.txt", "config": "config.json",
	}}
	if cfg.Head == "rnnt" {
		m.Family, m.Files["step"] = FamilyGigaAMRNNT, "step.mlmodelc"
	}
	t0 := time.Now()
	rec, err := Load(context.Background(), m, filepath.Clean(dir), asr.Variant{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	t.Logf("loaded in %v", time.Since(t0).Round(time.Millisecond))

	// The pipeline hands over eight segments at a time; a CoreML build is
	// better fed more at once, and this measures it fed everything.
	t0 = time.Now()
	got, err := rec.Decode(context.Background(), batch, 16000, asr.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(t0)

	edits, chars := 0, 0
	for i, g := range got {
		r := norm(refs[i])
		edits += levenshtein([]rune(norm(g.Text)), []rune(r))
		chars += utf8.RuneCountInString(r)
		if len(g.Tokens) != len(g.Timestamps) || len(g.Tokens) != len(g.LogProbs) {
			t.Fatalf("segment %d: %d tokens, %d timestamps, %d log-probs", i, len(g.Tokens), len(g.Timestamps), len(g.LogProbs))
		}
	}
	cer := float64(edits) / float64(chars)
	t.Logf("%d segments, %.0f s of speech in %v (%.0fx realtime), CER vs reference %.2f%%",
		len(batch), speech, elapsed.Round(time.Millisecond), speech/elapsed.Seconds(), 100*cer)
	t.Logf("first: %q", got[1].Text)
	if cer > 0.01 {
		t.Errorf("CER %.2f%% against the reference", 100*cer)
	}
}

var punct = regexp.MustCompile(`[^\p{L}\p{N}\s]`)

func norm(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	return strings.Join(strings.Fields(punct.ReplaceAllString(s, "")), " ")
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			sub := prev[j-1]
			if a[i-1] != b[j-1] {
				sub++
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, sub)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
