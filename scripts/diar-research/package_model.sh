#!/usr/bin/env bash
#
# Pack the exported graphs into the archives the catalog downloads:
#
#   nemotron-3-diarization-<rev>.tar.gz       fp32, the default
#   nemotron-3-diarization-int8-<rev>.tar.gz  int8 step graph, opt-in
#
# Each holds the two graphs, the filterbank and silence embedding, config.json
# (the offline pass's parameters, read off the model by golden_sortformer.py),
# the model's licence and a NOTICE of where it came from, as OpenMDW-1.1 asks.
#
#   venv-hf/bin/python export_onnx.py --out build/fp32
#   venv/bin/python quantize_int8.py --src build/fp32/step.onnx --out build/int8-step.onnx
#   ./package_model.sh build/fp32 build/int8-step.onnx build/
#
# Then upload both to the models-nemotron-diar-1 release and put the printed
# sha256 and sizes into internal/registry/catalog.yaml.
set -euo pipefail

FP32="${1:?exported graph directory}"
INT8_STEP="${2:?int8 step.onnx}"
OUT="${3:?output directory}"
REV="${REV:-2026-09-23}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$HERE/../.."

pack() {
  local name="$1" step="$2" variant="$3"
  local stage
  stage="$(mktemp -d)"
  local dir="$stage/$name"
  mkdir -p "$dir"
  cp "$FP32/embed.onnx" "$FP32/mel_filters.bin" "$FP32/silence_embeds.bin" "$dir/"
  cp "$step" "$dir/step.onnx"
  cp "$ROOT/testdata/golden/sortformer/reference.json" "$dir/config.json"
  cp "$HERE/LICENSE-OpenMDW-1.1.txt" "$dir/LICENSE"
  cat > "$dir/NOTICE" <<NOTICE
Nemotron-3-Diarization, by NVIDIA Corporation.
https://huggingface.co/nvidia/Nemotron-3-Diarization
Revision $(python3 -c "import json;print(json.load(open('$dir/config.json'))['model_revision'])"), licensed under OpenMDW-1.1 (LICENSE).

Converted to ONNX for NanoASR (https://github.com/usunrise88/NanoASR) with
scripts/diar-research/export_onnx.py, through transformers
$(python3 -c "import json;print(json.load(open('$dir/config.json'))['transformers_revision'])").
$variant
NOTICE
  # fixed order and metadata, so the same inputs give the same archive
  tar --sort=name --owner=0 --group=0 --numeric-owner --mtime='2026-09-23 00:00Z' \
    -C "$stage" -cf - "$name" | gzip -n -9 > "$OUT/$name.tar.gz"
  rm -rf "$stage"
  printf '%s  sha256 %s  size_bytes %s\n' "$name.tar.gz" \
    "$(sha256sum "$OUT/$name.tar.gz" | cut -d' ' -f1)" "$(stat -c %s "$OUT/$name.tar.gz")"
}

mkdir -p "$OUT"
pack "nemotron-3-diarization-$REV" "$FP32/step.onnx" "The graphs are float32, unquantized."
pack "nemotron-3-diarization-int8-$REV" "$INT8_STEP" \
  "step.onnx is dynamically quantized to int8 (per-channel weights) by quantize_int8.py."
