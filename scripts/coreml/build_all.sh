#!/usr/bin/env bash
#
# Build every CoreML model in the catalog from its authors' sources and pack
# the archives the models-coreml-1 release carries:
#
#   scripts/coreml/build_all.sh [OUT_DIR] [--update-catalog]
#
# OUT_DIR defaults to dist/models-coreml-1. With --update-catalog the printed
# sha256 and sizes are also written into internal/registry/catalog.yaml.
# Needs macOS 15 or later (compiling a model needs Core ML) and the
# environment in requirements.txt; NANOASR_COREML_WORK (default .cache/coreml)
# holds the sources, the weights and the intermediates, about 15 GB.
#
# MODELS limits the build to some of: gigaam silero sortformer voxlock
# campplus tone nemotron.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="$ROOT/dist/models-coreml-1"
UPDATE=()
for a in "$@"; do
  case "$a" in
    --update-catalog) UPDATE=(--update-catalog) ;;
    *) OUT="$a" ;;
  esac
done
MODELS="${MODELS:-gigaam silero sortformer voxlock campplus tone nemotron}"
PYTHON="${PYTHON:-python3}"

# The catalog entries each converter packages (macOS's bash 3 has no maps).
ids_of() {
  case "$1" in
    gigaam) echo gigaam-v3-rnnt-punct-ru-coreml gigaam-v3-ctc-punct-ru-coreml ;;
    silero) echo silero-vad-v5-coreml ;;
    sortformer) echo nemotron-3-diarization-coreml ;;
    voxlock) echo voxlock-int8-coreml ;;
    campplus) echo campplus-sv-zh-en-coreml ;;
    tone) echo t-one-ctc-ru-coreml ;;
    nemotron) echo nemotron-3.5-streaming-multi-coreml ;;
    *) echo "unknown model $1" >&2; return 1 ;;
  esac
}

"$HERE/fetch.sh"
ids=()
for m in $MODELS; do
  more="$(ids_of "$m")"
  # shellcheck disable=SC2206 # ids have no spaces
  ids+=($more)
done
for m in $MODELS; do
  echo "== $m"
  "$PYTHON" "$HERE/$m.py"
done
"$PYTHON" "$HERE/pack.py" "$OUT" "${ids[@]}" ${UPDATE[@]+"${UPDATE[@]}"}
