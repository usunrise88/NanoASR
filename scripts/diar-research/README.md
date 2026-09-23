# Nemotron-3-Diarization: conversion, verification and CPU measurements

Research tooling for putting NVIDIA's Sortformer diarization model behind
`internal/diarize.Diarizer`. Nothing here ships: no Go code depends on it, and
`make test` does not run it. It is committed because the measurements below
decide how that work is done, and because a claim nobody can re-run decays into
folklore within a release or two.

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
`[cache 264 | FIFO 40 | chunk 340 | right context 40]` = 684 frames.

| mode | RTF | peak RSS | quality |
|---|---|---|---|
| chunked fp32, the published offline config | 0.0233 | 841 MB | the chunked path itself |
| **one window fp32** | **0.0116** | 841 MB | no speaker-cache approximation |
| one window int8 per-channel | 0.0083 | 320 MB | see below |
| *this server's current diarization* | *0.073* | | |

**The win is structural, not arithmetic.** A chunked step reads 684 frames to
advance 340: half the work is context it re-reads. One window pays none of it,
and skips the speaker cache — a lossy approximation of the attention the model
would rather have — entirely. Graph optimization in onnxruntime changes nothing
here (685 ms vs 687 ms with `ORT_DISABLE_ALL`): the graph is already flat.

Cost grows quadratically with the window, so one window stops winning around
four minutes of audio:

| T | audio | fp32 one window | fp32 chunked |
|---|---|---|---|
| 684 | 54.7s | 0.0116 | 0.0269 |
| 1536 | 122.9s | 0.0149 | 0.0192 |
| 3072 | 245.8s | 0.0213 | 0.0240 |

RoPE caps a window at `pos_emb_max_len` 5000 frames, i.e. 400 s.

### On quantization

`quantize_variants.py` beats the published int8 by using per-channel weight
scales, and by leaving `reduce_range` off: this CPU has VNNI, so the int8
accumulate path does not saturate and the sacrificed bit buys nothing.

| variant | RTF | max Δp | decisions vs fp32 |
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
venv/bin/python bench_window.py      # where one window stops beating chunking
venv/bin/python bench_memory.py models/step.onnx 684
venv/bin/python decision_drift.py
venv/bin/python diarize_once.py      # one window end to end, prints the turns
```

`prep_input.py` and the `verify_*` scripts need the `.nemo` checkpoint in
`models/` and NeMo importable. That means NeMo from git `main` plus
`lhotse==2.0.0a6`; the benchmarks need none of it. Benchmarks are measurements,
not tests: they report numbers and assert nothing, because the numbers depend on
the machine.

## What this leaves to do

A mel front end in Go (`mel_filters.bin` plus pre-emphasis 0.97, n_fft 512, Hann
400 centred in 512, hop 160, `log(x + 2^-24)`, no normalization), two
onnxruntime sessions, thresholding into `diarize.Turn`, and the existing
per-word attribution unchanged. The speaker-cache state machine is only needed
above roughly four minutes of audio, and can wait for a recording that long.
