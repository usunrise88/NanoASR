#!/usr/bin/env bash
#
# Fetch the models the development loop and the integration tests need.
#
# This is NOT the model registry: it is a developer convenience that puts two
# models where a local registry can find them. Checksums are pinned because an
# unverified download is a supply-chain hole even in a dev script.
set -euo pipefail

MODELS_DIR="${MODELS_DIR:-.models}"
BASE="https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models"

GIGAAM_ARCHIVE="sherpa-onnx-nemo-ctc-giga-am-v2-russian-2025-04-19.tar.bz2"
GIGAAM_SHA="777be8717d8aaf04861823671290f7687f7579fd9ac63a2124955573f920caf5"
GIGAAM_DIR="sherpa-onnx-nemo-ctc-giga-am-v2-russian-2025-04-19"

SILERO_FILE="silero_vad.onnx"
SILERO_SHA="9e2449e1087496d8d4caba907f23e0bd3f78d91fa552479bb9c23ac09cbb1fd6"

log() { printf '==> %s\n' "$*" >&2; }

verify() {
  local file="$1" want="$2"
  local got
  got="$(sha256sum "$file" | cut -d' ' -f1)"
  if [[ "$got" != "$want" ]]; then
    echo "checksum mismatch for $file" >&2
    echo "  expected $want" >&2
    echo "  got      $got" >&2
    exit 1
  fi
}

mkdir -p "$MODELS_DIR"

# --- GigaAM v2 CTC (Russian) -------------------------------------------------
if [[ -f "$MODELS_DIR/gigaam-v2-ctc-ru/model.yaml" ]]; then
  log "gigaam-v2-ctc-ru already present"
else
  log "downloading GigaAM v2 CTC (~160 MB)"
  curl -fSL --retry 3 -o "$MODELS_DIR/$GIGAAM_ARCHIVE" "$BASE/$GIGAAM_ARCHIVE"
  verify "$MODELS_DIR/$GIGAAM_ARCHIVE" "$GIGAAM_SHA"

  # Unpack beside the target and rename, so an interrupted run never leaves a
  # half-populated model directory that the registry would happily load.
  rm -rf "$MODELS_DIR/$GIGAAM_DIR" "$MODELS_DIR/.gigaam.tmp"
  tar xjf "$MODELS_DIR/$GIGAAM_ARCHIVE" -C "$MODELS_DIR"
  mv "$MODELS_DIR/$GIGAAM_DIR" "$MODELS_DIR/.gigaam.tmp"

  # modeling_unit is char because tokens.txt is a 34-entry Russian character
  # vocabulary whose first entry is a literal space, not a SentencePiece model.
  # features.dim is 64: GigaAM's front end is not the 80-bin default, and a
  # mismatch produces confident nonsense rather than an error.
  cat > "$MODELS_DIR/.gigaam.tmp/model.yaml" <<'YAML'
id: gigaam-v2-ctc-ru
revision: "2025-04-19"
kind: asr
family: nemo_ctc
display_name: GigaAM v2 CTC (Russian)
languages: [ru]
sample_rate: 16000
modeling_unit: char
files:
  model: model.int8.onnx
  tokens: tokens.txt
features:
  sample_rate: 16000
  dim: 64
runtime:
  decoding_method: greedy_search
resources:
  approx_rss_mb: 900
license: "GigaAM license — see LICENSE in this directory"
notes: >-
  Character-level Russian CTC. Trained at 16 kHz; 8 kHz telephony is upsampled
  before recognition, which is a quality trade-off worth measuring on real data.
YAML

  rm -rf "$MODELS_DIR/gigaam-v2-ctc-ru"
  mv "$MODELS_DIR/.gigaam.tmp" "$MODELS_DIR/gigaam-v2-ctc-ru"
  rm -f "$MODELS_DIR/$GIGAAM_ARCHIVE"
  log "gigaam-v2-ctc-ru ready"
fi

# --- Silero VAD --------------------------------------------------------------
if [[ -f "$MODELS_DIR/silero-vad-v5/model.yaml" ]]; then
  log "silero-vad already present"
else
  log "downloading Silero VAD"
  mkdir -p "$MODELS_DIR/.silero.tmp"
  curl -fSL --retry 3 -o "$MODELS_DIR/.silero.tmp/$SILERO_FILE" "$BASE/$SILERO_FILE"
  verify "$MODELS_DIR/.silero.tmp/$SILERO_FILE" "$SILERO_SHA"

  cat > "$MODELS_DIR/.silero.tmp/model.yaml" <<'YAML'
id: silero-vad-v5
revision: "5"
kind: vad
family: silero_vad
display_name: Silero VAD v5
languages: [multi]
sample_rate: 16000
files:
  model: silero_vad.onnx
resources:
  approx_rss_mb: 16
license: mit
YAML

  rm -rf "$MODELS_DIR/silero-vad-v5"
  mv "$MODELS_DIR/.silero.tmp" "$MODELS_DIR/silero-vad-v5"
  log "silero-vad ready"
fi

log "models in $MODELS_DIR:"
ls -1 "$MODELS_DIR"
