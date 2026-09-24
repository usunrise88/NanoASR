# Nemotron-3-Diarization: conversion, verification and CPU measurements

Research tooling behind `internal/diarize/sortformer`, the Go diarizer built on
NVIDIA's Sortformer model. No Go code imports it and `make test` does not run
it, but three of these scripts produce what ships or what the Go tests check:
`export_onnx.py` and `quantize_int8.py` make the graphs, `package_model.sh`
packs the catalog's archives, and `golden_sortformer.py` writes
`testdata/golden/sortformer`. The rest is committed because the measurements
below decided how the work was done, and because a claim nobody can re-run
decays into folklore within a release or two.

Python, not Go, on purpose. Conversion happens once on a developer machine; the
runtime stays onnxruntime on CPU, as it already is for every `nemo_ctc` model
this server serves.

## The model

`nvidia/Nemotron-3-Diarization`, released 23 September 2026. A 31-layer
Transformer encoder with RoPE, 100M parameters, 128 mel bins in, per-speaker
activity out for up to 8 speakers at 10 ms resolution. Speakers are ordered by
first arrival, so there is no clustering step and no threshold to tune — which
is the reason to care about it at all.

Licensed OpenMDW-1.1: permissive, attribution on redistribution, no restriction
on outputs or on hardware.

## Where the graphs come from

From `transformers`, not from NeMo, via
[NealCaren/Nemotron-3-Diarization-ONNX](https://huggingface.co/NealCaren/Nemotron-3-Diarization-ONNX):

| file | what it is |
|---|---|
| `embed.onnx` | feature stacking + projection: `[1, N, 128]` → `[1, ceil(N/8), 512]` |
| `step.onnx` | encoder + head: `[1, T, 512]` → `[1, 8T, 8]` logits at 10 ms |
| `step_int8.onnx` | the same, dynamically quantized |
| `mel_filters.bin` | float32 `[128, 257]` Slaney mel filterbank |
| `silence_embeds.bin` | float32 `[512]` learned silence embedding |

`export_onnx.py` repeats that export with the model and `transformers`
revisions pinned, and its output is byte-identical to the published graphs —
so the archives in the catalog are ones we produced, not ones we downloaded.

`export_from_nemo.py` is the route we did **not** take, kept as the reason why.
Exporting from NeMo requires its `main` branch (the released `nemo_toolkit`
3.0.0 does not know `self_attention_model='rope'` and cannot even load the
checkpoint), a pre-release `lhotse==2.0.0a6`, and `flexpatch.py` — because the
production encoder computes attention with `torch.nn.attention.flex_attention`,
whose `create_block_mask` runs `vmap` and traces to ONNX under no exporter.
The `transformers` implementation takes `attn_implementation="eager"` and needs
none of that.

## Verify before you trust

The artifacts above are a third party's. `verify_artifacts.py` and
`verify_step_graph.py` check them against the `.nemo` checkpoint — the same
weights the exporter started from — rather than against their own README:

| artifact | checked against | difference |
|---|---|---|
| `silence_embeds.bin` | `sortformer_modules.learnable_sil_emb` | 0.000e+00 |
| `embed.onnx` | `SortformerEncLabelModel._call_pre_encode` | 7.9e-06 |
| `step.onnx` | encoder + head of the checkpoint | 3.4e-09 |

That check found something no README states: **`step.onnx` takes no lengths, so
padded positions must never be fed to it.** Comparing with a half-valid speaker
cache — which NeMo masks and this graph cannot — shows a difference of 1.2e-02;
with a densely packed sequence it is 3.4e-09. A Go implementation that pads will
be quietly wrong.

## What the measurements say

Four cores, Xeon 2.8 GHz with `avx512_vnni`, one step of
`[cache 264 | FIFO 40 | chunk 340 | right context 40]` = 684 frames, which
advances the recording by one 340-frame chunk, 27.2 s. RTF below is always
time per step over those 27.2 s.

| mode | step | RTF | peak RSS |
|---|---|---|---|
| fp32, Python onnxruntime 1.30 | 645 ms | 0.0237 | 841 MB |
| fp32, Go, the bundled onnxruntime 1.27.1 | 707 ms | 0.0260 | ~500 MB |
| int8 per-channel, Go | 477 ms | 0.0175 | ~210 MB |
| *sherpa diarization, the previous default* | | *0.14* | |

Graph optimization in onnxruntime changes nothing here (685 ms vs 687 ms with
`ORT_DISABLE_ALL`): the graph is already flat. Letting idle onnxruntime threads
sleep instead of spin (`session.intra_op.allow_spinning=0`) costs 10%.

**Correction: one window is not the reference.** An earlier version of this
README reported "one window fp32" at RTF 0.0116 as the main win, and said the
speaker cache was only needed above four minutes. Both were wrong. 0.0116 was
the same 684-frame step divided by the 54.7 s it spans rather than the 27.2 s
it advances — and the int8 figure had the same double denominator (0.0083 vs
0.0165). More importantly, the reference's offline forward *is* the chunked
pass with the speaker cache: its output equals one window only up to one chunk,
27.2 s, and the cache is compressed from the first update of anything longer.
The model was trained on sessions of up to 105 s; one window over minutes of
audio is a different computation, not a faster route to the same one. The Go
diarizer implements the chunked pass.

### On quantization

`quantize_variants.py` beats the published int8 by using per-channel weight
scales, and by leaving `reduce_range` off: this CPU has VNNI, so the int8
accumulate path does not saturate and the sacrificed bit buys nothing.

| variant | RTF (per 27.2 s step) | max Δp | decisions vs fp32 |
|---|---|---|---|
| int8 as published | 0.0145 | 0.138 | 99.7601% |
| int8 per-channel | 0.0165 | 0.086 | 99.7990% |
| int8 per-channel + reduce_range | 0.0154 | 0.253 | 98.9104% |

"99.8% of decisions agree" is not the whole answer, so `decision_drift.py` asks
where they differ. Every single flip lands on a frame where fp32 itself reported
a probability within ±0.05 of the 0.5 threshold, median margin 0.013.
Quantization never contradicts a confident decision; it tips frames the model
was undecided about, which moves a segment boundary by a frame or two. On a
machine without VNNI this conclusion would differ.

## Running it

Put the artifacts in `models/` next to where you run from, then:

```bash
python -m venv venv && venv/bin/pip install \
    --index-url https://download.pytorch.org/whl/cpu torch numpy
venv/bin/pip install onnx onnxruntime soundfile

venv/bin/python prep_input.py        # real speech -> mel -> embeddings
venv/bin/python verify_artifacts.py  # the third-party files against the checkpoint
venv/bin/python verify_step_graph.py
venv/bin/python quantize_variants.py
venv/bin/python bench_step.py '[{"name":"fp32","path":"models/step.onnx","threads":4}]'
venv/bin/python bench_window.py      # step cost against window length
venv/bin/python bench_memory.py models/step.onnx 684
venv/bin/python decision_drift.py
venv/bin/python diarize_once.py      # one window end to end (not the reference; see above)
```

`prep_input.py` and the `verify_*` scripts need the `.nemo` checkpoint in
`models/` and NeMo importable. That means NeMo from git `main` plus
`lhotse==2.0.0a6`; the benchmarks need none of it. Benchmarks are measurements,
not tests: they report numbers and assert nothing, because the numbers depend on
the machine.

## What became of it

`internal/diarize/sortformer`, and the numbers its tests hold it to:

- **Front end** (`frontend.go`, `fft.go`): the `transformers` extractor's
  log-mel in float64, computed per frame from the samples so a chunk's features
  come out bitwise equal to a whole-file pass. Within 6.1e-5 of the reference.
- **Chunk loop and speaker cache** (`loop.go`, `cache.go`), ported line by line
  from `Nemotron3DiarizationSpeakerCache`. Driven by a fake network whose every
  logit is known (splitmix64, identical in Go and Python), every step's input
  and the cache and FIFO after every update equal the reference's.
- **The whole pass** over 34.7 s of speech, cache compressed: speaker
  probabilities within 3e-6 of the reference.
- **DER** on the 16-minute dialogue of `make diar-eval`: 1.4% with no speaker
  count, fp32 and int8 alike, against 5.9% (2.9% given the count) for the
  previous sherpa default.
- **One onnxruntime** in the process: the binding loads the copy sherpa-onnx
  already linked, checked through `/proc/self/maps`, and both run side by side
  under `-race`.
- **One shared session**: two jobs on it get 0.76-0.81 of the throughput of
  two sessions, and a second session would cost another ~450 MB of weights.
