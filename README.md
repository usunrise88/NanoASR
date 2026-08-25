# NanoASR

**English** · [Русский](README.ru.md)

Offline speech recognition server built on [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx).
A single binary, CPU inference, word-level timestamps, three HTTP API dialects and an
embedded web interface.

The service targets batch processing of recorded audio: calls, interviews and meetings
from one minute to half an hour, mono and stereo, including 8 kHz telephony. No audio
leaves the machine — models run locally, and network access is required only to fetch
model weights initially.

## Architecture

A request passes through a fixed pipeline. Each stage is measured separately and
reported in the `stats.stages_ms` field of the response.

```
                                  ┌──────────────┐
file ──► decode ──► vad ──► asr ──►   assemble   ──► diarize ──► post ──► result
         │          │       │      │              │   │           │
    ffmpeg or   Silero  sherpa-onnx  tokens    pyannote +    punctuation,
    native      VAD v5  in batches   into words embeddings   ITN, hotwords
    WAV/PCM             per segment  with timings
```

| Stage | Purpose |
|---|---|
| `decode` | Format detection, decoding, resampling to 16 kHz, channel handling |
| `vad` | Speech segmentation and silence detection |
| `asr` | Batched recognition of segments under a CPU governor |
| `assemble` | Assembly of tokens into words with time boundaries |
| `diarize` | Speaker attribution and segment splitting at turn boundaries |
| `post` | Punctuation and inverse text normalisation |

Key implementation properties:

- **Single entry point.** All business logic sits behind the `core.Service` interface.
  API dialects translate request and response shapes and have no access to the model
  pool, the registry or the queue. Adding a dialect means adding one package that
  registers itself from `init()`.
- **Model pool.** Resident instances with reference counting, LRU eviction and hot
  revision swapping that does not interrupt in-flight requests.
- **CPU governor.** The number of concurrently executing batches is bounded by a
  weighted semaphore, so parallel requests do not degrade throughput to zero.
- **Job queue.** SQLite (pure Go, no cgo) for metadata, a file spool for audio,
  priorities, resumption after restart, an SSE event stream and HMAC-signed webhooks.
- **One artefact.** The web interface is embedded into the binary at compile time.

## Features

**Recognition**

- Word-level timestamps with per-word confidence.
- Model families: `nemo_ctc`, `zipformer_ctc`, `wenet_ctc`, `transducer`, `telespeech`.
- Segment batching bounded by count and total duration.
- Per-request model selection; several models resident in parallel.
- `greedy_search` and `modified_beam_search` decoding methods.
- Recognition biasing towards a supplied word list (hotwords).

**Audio**

- WAV and raw PCM are decoded in-process with no external dependencies: 16/24/32-bit,
  float32, a-law and µ-law.
- Every other format is handled through ffmpeg, which remains optional.
- Polyphase resampling to 16 kHz.
- Three channel handling modes: downmix, first channel, per-channel recognition.

**Results**

- Text, segments, words, silence spans, speakers, speech ratio and per-stage statistics.
- Response formats: JSON, plain text, SRT, VTT, TSV.
- Subtitles carrying speaker labels.

**Post-processing**

- Punctuation and casing, from the recognition model or from a dedicated punctuation model.
- Inverse text normalisation for Russian: numbers, dates, times, currency, phone
  numbers and percentages.
- Diarization: speaker attribution per word and segment splitting at turn boundaries.

**Operations**

- Three HTTP API dialects served concurrently on one port.
- API key authentication with per-key rate limits and priorities.
- Model registry: built-in catalog, SHA-256 verified downloads, management over the API.
- Automatic pool sizing from CPU count and available memory.
- `/healthz` and `/readyz` probes that reflect queue depth.

## Installation

### Release archives

Archives are published on the [releases page](https://github.com/usunrise88/NanoASR/releases).

| File | Platform |
|---|---|
| `nanoasr-<tag>-linux-amd64.tar.gz` | Linux, web interface included |
| `nanoasr-<tag>-linux-amd64-noui.tar.gz` | Linux, without the web interface |
| `nanoasr-<tag>-windows-amd64.zip` | Windows, web interface included |
| `nanoasr-<tag>-windows-amd64-noui.zip` | Windows, without the web interface |

The web interface is embedded at compile time, so the build without it is a separate
binary rather than a runtime flag.

The native libraries (`libonnxruntime`, `libsherpa-onnx`) are contained in the archive.
Unpack it in full: on Linux the binary's `RUNPATH` points at `./lib`, and on Windows the
loader looks for the DLLs next to the executable.

```bash
tar -xzf nanoasr-v1.0.0-linux-amd64.tar.gz -C ~/nanoasr
cd ~/nanoasr
```

### Docker

```bash
docker compose -f deploy/docker-compose.yml up --build
```

The image contains ffmpeg and the native libraries. Model weights live in the
`/var/lib/nanoasr` volume and are not baked into the image. The API key is supplied
through an environment variable:

```bash
NANOASR_AUTH_KEYS=sk-... docker compose -f deploy/docker-compose.yml up -d
```

### Building from source

Requires Go 1.24+, a C/C++ compiler (cgo is mandatory — sherpa-onnx is written in C++)
and Node 22+ for the web interface.

```bash
make web build        # web interface and server
make build-noui       # server without the web interface
make dist             # relocatable archive with the native libraries
make docker           # container image
```

ffmpeg is optional. Without it, WAV and PCM are available, including a-law and µ-law;
every other format is rejected with `415`.

## Getting started

### The `init` command

`nanoasr init` produces a working configuration, issues two API keys and downloads the
default models.

```bash
./nanoasr init
./nanoasr serve -config nanoasr.yaml
```

```
configuration  nanoasr.yaml
data           /var/lib/nanoasr

admin key      sk-nanoasr-...
user  key      sk-nanoasr-...
```

The data directory is `/var/lib/nanoasr` when it is writable and `./nanoasr-data`
otherwise. The chosen path is printed.

| Flag | Purpose |
|---|---|
| `-config FILE` | Path of the configuration to write, `nanoasr.yaml` by default |
| `-data-dir DIR` | Directory for models and the database |
| `-addr ADDR` | Listen address, `127.0.0.1:8080` by default |
| `-model ID` | Default recognition model |
| `-no-diarize` | Skip the diarization models |
| `-no-download` | Write the configuration only |
| `-force` | Overwrite an existing configuration file |

The configuration is written and validated by the loader before any weights are
downloaded, which rules out leaving behind a file the server would refuse to read.

### API keys

```bash
nanoasr key list                            # key listing, secrets masked
nanoasr key issue ci -rps 5                 # issue a key, secret printed to stdout
nanoasr key issue bot -hash                 # store only the sha256 digest
nanoasr key issue ops -admin -interactive   # administrative key outside the batch queue
nanoasr key remove ci                       # revoke a key
```

| Flag | Purpose |
|---|---|
| `-admin` | Access to model and configuration management |
| `-interactive` | This key's jobs are served ahead of the batch queue |
| `-rps N` | Request rate limit, `0` for unlimited |
| `-hash` | Store only the SHA-256 digest; the secret is printed once |

The configuration is edited as a YAML document: comments, key order and absent settings
are preserved. Writes are atomic and the result is validated before the file is
replaced. Keys are read at startup, so changes take effect after a server restart.

`auth.mode: open` is permitted only when the listen address is a loopback address.
`apikey` mode with an empty key list causes the server to refuse to start.

## Web interface

The single-page application is served at `/ui/` and is controlled by the `ui.enabled`
setting. It asks for an API key on the first `401` response.

| Screen | Purpose |
|---|---|
| Transcribe | File upload, model and option selection, progress tracking |
| Result | Player with word highlighting, speaker filter, search and export |
| Jobs | History, statuses, filters, cancellation, result review |
| Models | Catalog, weight downloads, residency, pinning, hot swap |
| Settings | Effective server configuration, read-only |

The interface runs entirely on top of the public API and has no endpoints of its own.

## Supported APIs

Dialects are enabled in the configuration and served concurrently:

```yaml
api:
  dialects: [openai, native, era]
```

### `native` — the full service interface

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/transcribe` | Synchronous recognition |
| `POST` | `/api/v1/jobs` | Enqueue a job |
| `GET` | `/api/v1/jobs` | History with cursor pagination |
| `GET` | `/api/v1/jobs/{id}` | Job state and result |
| `GET` | `/api/v1/jobs/{id}/events` | SSE event stream |
| `DELETE` | `/api/v1/jobs/{id}` | Cancel a job |
| `GET` | `/api/v1/models` | Resident and installed models |
| `GET` | `/api/v1/catalog` | Built-in model catalog |
| `POST` | `/api/v1/models/{id}/download` | Download weights, progress over SSE |
| `POST` | `/api/v1/models/{id}/load` | Load a model into memory |
| `POST` | `/api/v1/models/{id}/unload` | Unload a model |
| `POST` | `/api/v1/models/{id}/pin` | Protect from LRU eviction |
| `POST` | `/api/v1/models/{id}/reload` | Hot-swap a revision |
| `GET` | `/api/v1/config` | Effective configuration |

Model and configuration management requires an administrative key. Errors are returned
as RFC 9457 `application/problem+json`.

Request parameters: `model`, `language`, `response_format`, `channel_mode`,
`decoding_method`, `max_active_paths`, `diarize`, `num_speakers`, `punctuate`, `itn`,
`hotwords[]`, `hotwords_score`, `strict`, `webhook_url`.

Response formats: `json`, `text`, `srt`, `vtt`.

### `openai` — OpenAI Audio API compatibility

| Method | Path |
|---|---|
| `POST` | `/v1/audio/transcriptions` |
| `POST` | `/v1/audio/translations` |
| `GET` | `/v1/models` |
| `GET` | `/v1/models/{id}` |

`file`, `model`, `language`, `prompt`, `response_format` and
`timestamp_granularities[]` are supported. Response formats: `json`, `verbose_json`,
`text`, `srt`, `vtt`. The `verbose_json` response carries additional
`timestamp_source`, `silence`, `warnings` and `stats` fields; OpenAI clients ignore
fields they do not know.

Differences from the original API:

- `/v1/audio/translations` returns `501`: no translation models are shipped.
- `prompt` is interpreted as a comma-separated hotword list rather than as a language
  model prompt.
- `temperature` is accepted and has no effect: the decoders do not sample.

### `era` — whisper-asr-webservice compatibility

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/asr` | Synchronous recognition, response delivered as a file |
| `POST` | `/detect-language` | Language reporting |
| `POST` | `/asr_task` | Enqueue a job and return its identifier |
| `GET` | `/asr_task/{task_id}` | Poll for state and collect the result |

The file is supplied in the `audio_file` field and the remaining parameters in the
query string: `encode`, `task`, `language`, `initial_prompt`, `vad_filter`,
`word_timestamps`, `diarize`, `min_speakers`, `max_speakers`, `output`.

`output` formats: `txt` (default), `vtt`, `srt`, `tsv`, `json`. All are served as
`text/plain` with the `Asr-Engine` and `Content-Disposition` headers.

Differences from the original service:

| Parameter or behaviour | NanoASR implementation |
|---|---|
| `task=translate` | `501`, no translation models are shipped |
| `initial_prompt` | Comma-separated hotword list |
| `encode`, `vad_filter` | Accepted, no effect: both are server settings here |
| `min_speakers`, `max_speakers` | Reduced to an exact speaker count; a range is dropped |
| `task_id` | Job identifier with the requested output format appended |
| `GET /` | Not served: the redirect to `/docs` is not implemented |
| Authentication | Required, as for every other dialect |

This contract has no field for warnings, so applied limitations are reported in the
`X-NanoASR-Warnings` header as a list of codes.

`/detect-language` returns the language declared by the recognition model that the
request resolved to. `confidence` is `1` when the model supports a single language and
`0` when it lists several and the first was taken. No language identification model is
shipped.

## Recognition settings

### `channel_mode` — channel handling

| Value | Behaviour |
|---|---|
| `downmix` (default) | Channels are averaged into a single track |
| `first` | Channel zero is used, the rest are discarded |
| `split` | Each channel is recognised separately, results merged by time |

`split` is intended for stereo recordings where the speakers are separated by channel
(a telephony A/B leg recording). Every segment and every word carries a `channel` field,
which separates speakers without diarization and more accurately than it. Diarization is
not run under `split` and returns the `diarization_skipped_split` warning.

`first` discards the remaining channels entirely and applies when the second channel is
known to carry no speech.

Under `split` each channel is decoded in full, so memory scales with the channel count.
The upper bound is set by `audio.max_split_channels` (2 by default); a file with more
channels is rejected with `400`.

### `decoding_method` — decoding strategy

| | `greedy_search` | `modified_beam_search` |
|---|---|---|
| Model families | All | `transducer` only |
| Principle | The most probable token is taken at each step | `max_active_paths` hypotheses are explored in parallel |
| Relative cost | Baseline | +29 % at 4 paths, +57 % at 8 paths |
| Hotword support | No | Required |

`greedy_search` is the default. `modified_beam_search` improves accuracy on noisy audio,
narrowband channels and rare proper nouns; on clean material the resulting text may be
identical.

CTC models do not support hypothesis search. A `modified_beam_search` request against
such a model is served with the model's own method and carries the
`decoding_method_unavailable` warning.

`decoding_method` and `max_active_paths` are fixed when a recogniser is loaded, so
varying them per request requires a second resident instance of the model. The budget is
set by `asr.variants.max`; at zero the request is served with the model's own settings
and a corresponding warning.

### `hotwords` — recognition biasing

A list of words and phrases whose probability is raised during decoding: proper nouns,
organisation names, terminology. The mechanism adjusts probabilities rather than the
vocabulary: a listed phrase may still be absent from the result, or may appear where it
was not spoken.

Preconditions:

1. The model family is `transducer`.
2. `decoding_method: modified_beam_search`.
3. The modelling unit can tokenise the phrase: `cjkchar` requires nothing further, while
   `bpe` and `cjkchar+bpe` require a `bpe_vocab` file in the model manifest.

The recommended list size is tens of phrases. The limit is set by accuracy rather than
performance: the longer the list, the more often the bias applies in the wrong place.
Only phrases the model actually gets wrong belong in the list.

The list forms part of the model pool key: each distinct hotword set requires its own
resident instance. The mechanism is meant for a stable vocabulary, not for a parameter
that varies between requests.

```yaml
asr:
  variants:
    max: 1
    allow_hotwords: true
postproc:
  hotwords:
    enabled: true
    default_score: 1.5
```

`hotwords_score` controls the strength of the bias; 1.5–2.0 is the working range. In the
OpenAI dialect the list arrives through `prompt` without a score, and
`postproc.hotwords.default_score` applies.

The models in the built-in catalog do not meet the preconditions above: the Russian
models use `char` or subwords without a vocabulary file. A catalog entry carrying
`bpe_vocab` is required for the mechanism to take effect.

## Diarization

Diarization runs as a second pass over the whole recording: a segmentation model finds
turn boundaries, an embedding model converts fragments into voice vectors, and
clustering groups the fragments belonging to one speaker.

The result carries `speaker` and `speaker_confidence` on both words and segments, along
with a `speakers[]` summary of speech duration and segment counts. Segments are split at
turn boundaries, and fragments too short to be a turn are absorbed into their neighbours.

```yaml
diarization:
  enabled: true
  segmentation_model: pyannote-segmentation-3
  embedding_model: campplus-sv-voxceleb
  clustering:
    num_clusters: 0
    threshold: 0.4
  min_duration_on: 0.3
  min_duration_off: 0.5
```

| Setting | Purpose |
|---|---|
| `segmentation_model` | Model that finds turn boundaries |
| `embedding_model` | Model that produces voice vectors |
| `clustering.num_clusters` | Fixed speaker count; `0` clusters by threshold |
| `clustering.threshold` | Cosine distance threshold when the speaker count is unknown |
| `min_duration_on` | Minimum turn duration, seconds |
| `min_duration_off` | Minimum gap between turns, seconds |

The `num_speakers` request parameter sets the speaker count for a single file. It does
not guarantee that many speakers: clustering builds a complete-linkage dendrogram and
cuts it either at a height (`threshold`) or at a given number of leaves. When an outlier
is present, cutting by leaf count can separate the outlier instead. If fewer speakers
were separated than requested, the response carries the `diarization_fewer_speakers`
warning.

**The choice of embedding model** determines separation quality. `campplus-sv-zh-en` is
trained on Chinese and English material and produces a single cluster for similar
Russian voices at any threshold. The default is `campplus-sv-voxceleb`, trained on the
multilingual VoxCeleb corpus. For highly similar voices, `wespeaker-voxceleb-resnet34`
applies, at roughly twice the CPU cost.

When every utterance is attributed to one speaker, apply the following measures in
decreasing order of effectiveness:

1. Use `channel_mode: split` if the speakers are separated by channel.
2. Change the embedding model to `wespeaker-voxceleb-resnet34`.
3. Lower `clustering.threshold` to 0.35.
4. Set `num_speakers` when the speaker count is known exactly.

The sample rate of the diarization models is checked against
`audio.target_sample_rate` at startup; a mismatch prevents the server from starting.

## Example requests

`KEY` below is an API key issued by `nanoasr init` or `nanoasr key issue`.

**Synchronous recognition**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/transcribe \
  -F file=@call.wav
```

**Word timings and statistics**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/transcribe \
  -F file=@call.wav \
  | jq '{text, rtf: .stats.rtf, stages: .stats.stages_ms, words: .segments[0].words[:3]}'
```

**Diarization with a known speaker count**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/transcribe \
  -F file=@call.wav -F diarize=true -F num_speakers=2 \
  | jq '.speakers, [.segments[] | {start, end, speaker}]'
```

**Stereo with per-channel recognition**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/transcribe \
  -F file=@call.wav -F channel_mode=split \
  | jq '[.segments[] | {channel, start, text}]'
```

**Subtitles**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/transcribe \
  -F file=@call.wav -F diarize=true -F response_format=srt \
  -o call.srt
```

**Asynchronous job with progress tracking**

```bash
JOB=$(curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/jobs \
  -F file=@meeting.wav -F diarize=true -F itn=true | jq -r .id)

curl -s -N -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/api/v1/jobs/$JOB/events"

curl -s -H "Authorization: Bearer $KEY" \
  "http://localhost:8080/api/v1/jobs/$JOB" | jq .result.text
```

**Webhook notification**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/api/v1/jobs \
  -F file=@call.wav \
  -F webhook_url=https://example.com/nanoasr
```

**OpenAI dialect**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST http://localhost:8080/v1/audio/transcriptions \
  -F file=@call.wav -F response_format=verbose_json \
  | jq '.text, .words[:3]'
```

**era dialect**

```bash
curl -s -H "Authorization: Bearer $KEY" \
  -X POST "http://localhost:8080/asr?output=srt&diarize=true&min_speakers=2&max_speakers=2" \
  -F audio_file=@call.wav

TASK=$(curl -s -H "Authorization: Bearer $KEY" \
  -X POST "http://localhost:8080/asr_task?output=json&word_timestamps=true" \
  -F audio_file=@call.wav | jq -r .task_id)

curl -s -H "Authorization: Bearer $KEY" "http://localhost:8080/asr_task/$TASK"
```

**Server state**

```bash
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/readyz
```

## Model registry

Model weights are not shipped and are downloaded onto the target machine. Every catalog
entry is pinned by URL, size and SHA-256 checksum, and downloaded files are verified
before use.

```bash
nanoasr models catalog                       # available catalog entries
nanoasr models list                          # installed models
nanoasr models pull gigaam-v3-ctc-punct-ru   # download
nanoasr models inspect ./my-model --probe sample.wav
```

`models inspect` determines a model's family and vocabulary, extracts metadata from the
`.onnx` files and produces a draft manifest for adding the model to the registry. The
`--probe` flag tests each candidate `features.dim` value in a separate process.

Models are loaded on demand, evicted by LRU and can be hot-swapped. Concurrent residency
is bounded by `asr.max_resident_models` and `asr.max_model_rss_mb`.

### Default models

`nanoasr init` downloads four models, which together cover the full processing cycle for
Russian speech:

| Identifier | Purpose | Size |
|---|---|---|
| `gigaam-v3-ctc-punct-ru` | Recognition, Russian, with punctuation and casing | 163 MB |
| `silero-vad-v5` | Voice activity detection | 0.6 MB |
| `pyannote-segmentation-3` | Speaker segmentation | 7 MB |
| `campplus-sv-voxceleb` | Speaker embeddings | 30 MB |

The default recognition model produces punctuation and capitalisation itself, so no
separate punctuation model is required for Russian.

### Built-in catalog

| Identifier | Type | Language | Size | Commercial use |
|---|---|---|---|---|
| `gigaam-v3-ctc-punct-ru` | Recognition, CTC | ru | 163 MB | Unconfirmed |
| `gigaam-v3-rnnt-punct-ru` | Recognition, transducer | ru | 170 MB | Unconfirmed |
| `gigaam-v2-ctc-ru` | Recognition, CTC | ru | 167 MB | Unconfirmed |
| `gigaam-v2-rnnt-ru` | Recognition, transducer | ru | 172 MB | Unconfirmed |
| `zipformer-small-en` | Recognition, transducer | en | 112 MB | Permitted |
| `silero-vad-v5` | Voice activity detection | multi | 0.6 MB | Permitted |
| `pyannote-segmentation-3` | Speaker segmentation | multi | 7 MB | Permitted |
| `campplus-sv-voxceleb` | Speaker embeddings | multi | 30 MB | Permitted |
| `campplus-sv-zh-en` | Speaker embeddings | multi | 28 MB | Permitted |
| `wespeaker-voxceleb-resnet34` | Speaker embeddings | multi | 27 MB | Permitted |

"Unconfirmed" means the model archive carries no machine-readable licence text. The
`registry.strict_license` setting refuses to download weights whose commercial use is
not confirmed.

The catalog is extended with custom entries through `registry.catalog_url`, and mirrors
are configured with `registry.mirrors`.

## Licence

MIT — see [LICENSE](LICENSE) and [NOTICE](NOTICE).

Model weights are not part of this repository and are distributed under their own
licences, including non-commercial ones.
