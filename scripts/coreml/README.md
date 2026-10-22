# CoreML builds

How the `-coreml` entries in `internal/registry/catalog.yaml` are made: the
models NanoASR runs on Apple silicon's Neural Engine and GPU when
`asr.provider` is `coreml`. Each one is built here from its authors' own
sources and weights, at pinned revisions, and packed into the archive the
`models-coreml-1` release carries. No Go code imports anything here.

| Converter | Catalog entry | Built from | Runs on |
|---|---|---|---|
| `gigaam.py` | `gigaam-v3-ctc-punct-ru-coreml`, `gigaam-v3-rnnt-punct-ru-coreml` | GigaAM v3 `v3_e2e_ctc.ckpt`, `v3_e2e_rnnt.ckpt` | Neural Engine |
| `silero.py` | `silero-vad-v5-coreml` | the weights of `silero_vad.onnx` (Silero v4) | Neural Engine |
| `sortformer.py` | `nemotron-3-diarization-coreml` | `nvidia/Nemotron-3-Diarization` through transformers | Neural Engine |
| `voxlock.py` | `voxlock-int8-coreml` | Voxlock's fp32 `model.safetensors` | GPU (enrollment, front), Neural Engine (back) |
| `campplus.py` | `campplus-sv-zh-en-coreml` | 3D-Speaker's `campplus_cn_en_common.pt` | GPU (fbank), Neural Engine (network) |
| `tone.py` | `t-one-ctc-ru-coreml` | `t-tech/T-one` | GPU |
| `nemotron.py` | `nemotron-3.5-streaming-multi-coreml` | FluidInference's CoreML build, unchanged | Neural Engine |

Every converter's docstring says what it changes in the model and why. The
reasons repeat: the Neural Engine has no LSTM, argmax or wide-kernel
convolution, and computes in fp16, which holds neither a quiet power spectrum
nor a loud one. So the LSTMs are written out as matrix products, the spectra
are taken as `2 log m + log(1 + r^2)` and the mel sums as log-sum-exps, and
shapes are static, with masks where the Go side's lengths vary.

## Building

Needs macOS 15 or later on Apple silicon (compiling a model needs Core ML) and
Python 3.12:

```bash
uv venv --python 3.12 .venv-coreml
uv pip install --python .venv-coreml/bin/python -r scripts/coreml/requirements.txt
PYTHON=.venv-coreml/bin/python scripts/coreml/build_all.sh --update-catalog
```

That fetches the sources and weights (`fetch.sh`, about 1.8 GB, each file
checked against its sha256), runs every converter, and writes the eight
archives to `dist/models-coreml-1/`. `--update-catalog` writes their sha256 and
sizes into the catalog; without it they are only printed. `MODELS="gigaam
tone"` builds some of them. Everything else goes to `.cache/coreml`
(`NANOASR_COREML_WORK`), about 6 GB with the intermediates, and
Nemotron-3-Diarization's checkpoint to the Hugging Face cache. Once the
downloads are done a full build takes about seven minutes on an M1.

The same versions give the same bytes: coremltools' run-to-run differences
(random item ids, map order, the conversion date) are taken out before
compiling, and the archives have sorted names, no owners and a fixed time.
Other versions of coremltools or torch change the programs, and with them the
catalog's sha256.

To try a build before it is released, install it where the server keeps its
models, as the downloader would:

```bash
.venv-coreml/bin/python scripts/coreml/install_local.py /var/lib/nanoasr/models gigaam-v3-rnnt-punct-ru-coreml
```

## Publishing

The catalog downloads the archives from the release its URLs name, so the
release goes up on the commit whose catalog pins their sha256:

```bash
gh release create models-coreml-1 --target <commit> --latest=false --title models-coreml-1 --notes "…" dist/models-coreml-1/*.tar.gz
```

`--latest=false` matters: `install.sh` finds the application through
`releases/latest`, and without it this release would take that place. A
published archive is never replaced by different bytes: servers on an older
catalog still check the old sha256. A rebuild that changes them gets a new
`REVISION` in `common.py`, and with it new archive names.

## Checks

Each converter checks its rewrite against the original in torch before
converting: packed segments against standalone runs (GigaAM), the masked step
against the dense one (Sortformer), the filterbank against torchaudio's
(CAM++). `silero.py --check AUDIO.wav` and `campplus.py --onnx MODEL.onnx`
compare with the ONNX models sherpa-onnx runs. The converted models are
checked from Go by tests that skip unless they are pointed at a package
(`.cache/coreml/pkg/<id>`) and a recording; each test's comment names its
variables:

| Test | Package |
|---|---|
| `internal/asr/coremlasr` `TestDecodeMatchesReference` | GigaAM, against reference transcripts |
| `internal/vad` `TestCoreMLMatchesSherpa` | Silero, against sherpa-onnx's detector |
| `internal/diarize/sortformer` `TestOfflineCoreMLMatchesReference` (`-tags integration`) | Sortformer, against the golden reference |
| `internal/asr/coremlasr` `TestTOneStreams`, `TestNemotronStreams` | the two streaming recognisers |
