package coremlasr

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
)

// TestStreamResamplerMatchesWholeBuffer: pieces in, the same samples out as
// resampling the whole stream at once.
func TestStreamResamplerMatchesWholeBuffer(t *testing.T) {
	x := make([]float32, 16000*2+123)
	for i := range x {
		x[i] = float32(math.Sin(float64(i)*0.05) * 0.5)
	}
	want := audio.NewResampler(16000, 8000).Resample(x)
	r := newStreamResampler(16000, 8000)
	var got []float32
	for off := 0; off < len(x); off += 777 {
		got = append(got, r.push(x[off:min(off+777, len(x))])...)
	}
	got = append(got, r.flush()...)
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if d := math.Abs(float64(got[i] - want[i])); d > 1e-5 {
			t.Fatalf("sample %d: %g, want %g", i, got[i], want[i])
		}
	}
}

// stream feeds a recording in 100 ms pieces the way the realtime engine
// does — accept, decode while ready, cut at an endpoint — and returns every
// utterance's text.
func stream(t *testing.T, rec asr.Streaming, pcm audio.PCM, lang string) ([]string, time.Duration) {
	s, err := rec.NewStream()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetLanguage(lang)
	var texts []string
	var busy time.Duration
	step := pcm.SampleRate / 10
	flush := func() {
		if h := s.Hypothesis(); h.Text != "" {
			texts = append(texts, h.Text)
		}
		s.Reset()
	}
	for off := 0; off < len(pcm.Samples); off += step {
		s.Accept(pcm.SampleRate, pcm.Samples[off:min(off+step, len(pcm.Samples))])
		t0 := time.Now()
		for s.Ready() {
			rec.Decode([]asr.Stream{s})
		}
		busy += time.Since(t0)
		if s.Endpoint() {
			flush()
		}
	}
	s.Finish()
	for s.Ready() {
		rec.Decode([]asr.Stream{s})
	}
	flush()
	return texts, busy
}

func readWAV(t *testing.T, path string, seconds float64) audio.PCM {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pcms, err := audio.NewWAVDecoder().Decode(context.Background(), f,
		audio.Options{TargetSampleRate: 16000, ChannelMode: core.ChannelDownmix})
	if err != nil {
		t.Fatal(err)
	}
	p := pcms[0]
	if n := int(seconds * 16000); seconds > 0 && n < len(p.Samples) {
		p.Samples = p.Samples[:n]
	}
	return p
}

func joinRunes(texts []string) []rune {
	var out []rune
	for i, s := range texts {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, []rune(norm(s))...)
	}
	return out
}

// TestTOneStreams streams a recording through the CoreML T-one. With
// NANOASR_TONE_REF set to a file of the ONNX model's transcript of the same
// audio it also compares the two. Needs NANOASR_COREML_TONE and
// NANOASR_STREAM_WAV.
func TestTOneStreams(t *testing.T) {
	dir, wav := os.Getenv("NANOASR_COREML_TONE"), os.Getenv("NANOASR_STREAM_WAV")
	if dir == "" || wav == "" {
		t.Skip("NANOASR_COREML_TONE and NANOASR_STREAM_WAV are not set")
	}
	m := registry.Manifest{ID: "t-one-coreml", Family: FamilyTOne, ModelingUnit: "char", Streaming: true,
		Files: map[string]string{"model": "model.mlmodelc", "tokens": "tokens.txt"}}
	t0 := time.Now()
	rec, err := LoadTOne(context.Background(), m, dir, StreamingOptions{
		EnableEndpoint: true, Rule1MinTrailingSilence: 2.4, Rule2MinTrailingSilence: 1.2, Rule3MinUtteranceLength: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	t.Logf("loaded in %v", time.Since(t0).Round(time.Millisecond))
	pcm := readWAV(t, wav, 120)
	texts, busy := stream(t, rec, pcm, "")
	t.Logf("%.0f s streamed, %d utterances, %v decoding (%.0fx real time)", pcm.Duration(), len(texts), busy.Round(time.Millisecond), pcm.Duration()/busy.Seconds())
	if len(texts) == 0 {
		t.Fatal("no transcript")
	}
	t.Logf("first: %q", texts[0])
	if ref := os.Getenv("NANOASR_TONE_REF"); ref != "" {
		b, _ := os.ReadFile(ref)
		r := []rune(norm(string(b)))
		got := joinRunes(texts)
		cer := float64(levenshtein(got, r)) / float64(utf8.RuneCountInString(string(r)))
		t.Logf("CER vs the ONNX model's transcript: %.2f%%", 100*cer)
		if cer > 0.03 {
			t.Errorf("CER %.2f%%", 100*cer)
		}
	}
}

// TestNemotronStreams streams a Russian recording through the CoreML
// Nemotron. Needs NANOASR_COREML_NEMOTRON and NANOASR_STREAM_WAV.
func TestNemotronStreams(t *testing.T) {
	dir, wav := os.Getenv("NANOASR_COREML_NEMOTRON"), os.Getenv("NANOASR_STREAM_WAV")
	if dir == "" || wav == "" {
		t.Skip("NANOASR_COREML_NEMOTRON and NANOASR_STREAM_WAV are not set")
	}
	m := registry.Manifest{ID: "nemotron-coreml", Family: FamilyNemotron, ModelingUnit: "bpe", Streaming: true,
		Files: map[string]string{"encoder": "encoder.mlmodelc", "decoder_joint": "decoder_joint.mlmodelc",
			"tokens": "tokenizer.json", "metadata": "metadata.json"}}
	t0 := time.Now()
	rec, err := LoadNemotron(context.Background(), m, dir, StreamingOptions{
		EnableEndpoint: true, Rule1MinTrailingSilence: 2.4, Rule2MinTrailingSilence: 1.2, Rule3MinUtteranceLength: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	t.Logf("loaded in %v", time.Since(t0).Round(time.Millisecond))
	pcm := readWAV(t, wav, 120)
	texts, busy := stream(t, rec, pcm, "ru-RU")
	t.Logf("%.0f s streamed, %d utterances, %v decoding (%.0fx real time)", pcm.Duration(), len(texts), busy.Round(time.Millisecond), pcm.Duration()/busy.Seconds())
	if len(texts) == 0 {
		t.Fatal("no transcript")
	}
	for i, s := range texts[:min(3, len(texts))] {
		t.Logf("%d: %q", i, s)
	}
	cyr := 0
	for _, r := range joinRunes(texts) {
		if r >= 'а' && r <= 'я' {
			cyr++
		}
	}
	if cyr == 0 {
		t.Error("no Cyrillic in a Russian transcript")
	}
}
