package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/usunrise88/nanoasr/internal/asr"
	"github.com/usunrise88/nanoasr/internal/asr/sherpa"
	"github.com/usunrise88/nanoasr/internal/audio"
	"github.com/usunrise88/nanoasr/internal/core"
	"github.com/usunrise88/nanoasr/internal/registry"
	"github.com/usunrise88/nanoasr/internal/words"
)

// probeDims are the feature dimensions worth trying: 80 is sherpa-onnx's
// default and the common value, 64 is GigaAM's.
//
// Measured, and the two families behave completely differently. NeMo models
// carry their own front end and ignore the setting: GigaAM v2 CTC transcribes
// correctly even when told 13. Zipformer models declare a fixed input shape,
// and onnxruntime answers a mismatch by throwing a C++ exception that
// terminates the process — not a bad transcript, a SIGABRT.
//
// That is why each candidate runs in a child process. A tool that dies while
// answering the question it was built to answer is not much of a tool, and the
// crash itself is the most useful result it can report.
var probeDims = []int{80, 64}

// probeChildCommand is the hidden subcommand a probe child runs as.
const probeChildCommand = "probe-one"

func inspectModel(args []string) error {
	fs := flag.NewFlagSet("models inspect", flag.ExitOnError)
	probe := fs.String("probe", "", "audio file to transcribe with each candidate features.dim")

	dirs, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(dirs) != 1 {
		return fmt.Errorf("usage: nanoasr models inspect <model-dir> [--probe file.wav]")
	}
	dir := dirs[0]

	draft, err := registry.Inspect(dir)
	if err != nil {
		return err
	}

	out, err := yaml.Marshal(draft.Manifest)
	if err != nil {
		return err
	}
	fmt.Printf("# draft manifest for %s\n# review every line before adding it to the catalog\n%s\n", dir, out)

	if len(draft.Notes) > 0 {
		fmt.Fprintln(os.Stderr, "inferred:")
		for _, n := range draft.Notes {
			fmt.Fprintln(os.Stderr, "  - "+n)
		}
	}
	if len(draft.Unresolved) > 0 {
		fmt.Fprintln(os.Stderr, "\nyou must confirm:")
		for _, n := range draft.Unresolved {
			fmt.Fprintln(os.Stderr, "  - "+n)
		}
	}

	if *probe == "" {
		if draft.Manifest.Features.Dim == 0 {
			fmt.Fprintln(os.Stderr, "\nrun again with --probe <wav> to settle features.dim")
		}
		return nil
	}
	return runProbe(dir, draft.Manifest, *probe)
}

// runProbe transcribes the same clip under each candidate feature dimension.
//
// Judging which output is speech and which is noise takes a person a second and
// a program cannot do it at all — so the tool prints both and says nothing
// about which is right. What it can decide is whether the question matters:
// identical output everywhere means the model derives its own front end.
func runProbe(dir string, m registry.Manifest, wavPath string) error {
	fmt.Fprintf(os.Stderr, "\nprobing %s with each candidate features.dim:\n", wavPath)

	ctx := context.Background()
	results := make(map[int]string, len(probeDims))
	for _, dim := range probeDims {
		text, err := probeInChild(ctx, dir, wavPath, dim)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n  dim %2d: %v\n", dim, err)
			continue
		}
		results[dim] = strings.TrimSpace(text)
		fmt.Fprintf(os.Stderr, "\n  dim %2d: %s\n", dim, truncate(text, 300))
	}

	if len(results) < 2 {
		fmt.Fprintln(os.Stderr, "\nfewer than two candidates loaded, so the probe settled nothing.")
		fmt.Fprintln(os.Stderr, "Fix the errors above and run it again.")
		return nil
	}
	if identical(results) {
		fmt.Fprintln(os.Stderr, "\nevery candidate produced the same transcript: this model derives its")
		fmt.Fprintln(os.Stderr, "feature dimension itself and ignores the manifest value. Record the")
		fmt.Fprintln(os.Stderr, "documented dimension anyway — it costs nothing and other families use it.")
		return nil
	}

	fmt.Fprintln(os.Stderr, "\nthe candidates disagree, so this model does use the configured value.")
	fmt.Fprintln(os.Stderr, "Record the dimension whose output is language rather than noise.")
	return nil
}

// identical reports whether every successful probe produced the same text.
func identical(results map[int]string) bool {
	if len(results) < 2 {
		return false
	}
	var first string
	for _, v := range results {
		if first == "" {
			first = v
			continue
		}
		if v != first {
			return false
		}
	}
	return true
}

// probeInChild runs one candidate in a separate process, so a model that
// aborts on a dimension mismatch takes the child down rather than the tool.
func probeInChild(ctx context.Context, dir, wavPath string, dim int) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, self, "models", probeChildCommand,
		dir, wavPath, strconv.Itoa(dim))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", fmt.Errorf("the model rejected this dimension (%s): %s",
				exit.ProcessState.String(), firstLine(stderr.String()))
		}
		return "", err
	}
	return stdout.String(), nil
}

// probeChild is the child half: transcribe once under one dimension and print
// the result. Failures are the exit status; the parent interprets them.
func probeChild(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: nanoasr models %s <dir> <wav> <dim>", probeChildCommand)
	}
	dir, wavPath := args[0], args[1]
	dim, err := strconv.Atoi(args[2])
	if err != nil {
		return err
	}

	draft, err := registry.Inspect(dir)
	if err != nil {
		return err
	}
	m := draft.Manifest
	m.Features.Dim = dim
	if m.Features.SampleRate == 0 {
		m.Features.SampleRate = 16000
	}
	if m.ModelingUnit == "" {
		m.ModelingUnit = words.UnitBPE
	}

	pcm, err := decodeProbeAudio(wavPath)
	if err != nil {
		return err
	}

	ctx := context.Background()
	load := sherpa.NewLoader(sherpa.LoaderOptions{NumThreads: 2, SkipWarmup: true})
	text, err := transcribeWith(ctx, load, m, dir, pcm)
	if err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no diagnostic output"
	}
	return s
}

func decodeProbeAudio(path string) (audio.PCM, error) {
	f, err := os.Open(path)
	if err != nil {
		return audio.PCM{}, err
	}
	defer f.Close()

	router := audio.NewRouter(audio.NewWAVDecoder(),
		audio.NewFFmpegDecoder("ffmpeg", 60*time.Second))

	// Downmix, so there is exactly one track: the probe asks whether the
	// model's front end matches this audio, and one channel answers that.
	pcm, _, err := router.Decode(context.Background(), f, audio.Options{
		TargetSampleRate: 16000,
		ChannelMode:      core.ChannelDownmix,
		MaxDurationSec:   120,
	})
	if err != nil {
		return audio.PCM{}, err
	}
	return pcm[0], nil
}

// transcribeWith loads the model under one candidate configuration and decodes
// the clip in a single pass. No VAD and no batching: the question is whether
// the front end matches, and a whole-file decode answers it with less to go
// wrong.
func transcribeWith(
	ctx context.Context,
	load func(context.Context, registry.Manifest, string, asr.Variant) (asr.Recognizer, error),
	m registry.Manifest, dir string, pcm audio.PCM,
) (string, error) {
	rec, err := load(ctx, m, dir, asr.Variant{})
	if err != nil {
		return "", err
	}
	defer rec.Close()

	got, err := rec.Decode(ctx, [][]float32{pcm.Samples}, pcm.SampleRate)
	if err != nil {
		return "", err
	}
	if len(got) == 0 || strings.TrimSpace(got[0].Text) == "" {
		return "(nothing recognised)", nil
	}
	return got[0].Text, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
