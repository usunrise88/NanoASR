#!/usr/bin/env bash
#
# Fetch the models the development loop and the integration tests need.
#
# Everything this used to do by hand — pinned URLs, checksums, unpacking,
# hand-written manifests — now lives in internal/registry/catalog.yaml and is
# done by the server itself. Keeping a second copy here is how the two drift
# apart, so this is a thin wrapper and nothing more.
set -euo pipefail

MODELS="${MODELS:-gigaam-v2-ctc-ru gigaam-v2-rnnt-ru zipformer-small-en silero-vad-v5}"
CONFIG="${CONFIG:-configs/nanoasr.dev.yaml}"

if [[ -x dist/nanoasr ]]; then
  NANOASR=(dist/nanoasr)
else
  NANOASR=(go run ./cmd/nanoasr)
fi

# shellcheck disable=SC2086
"${NANOASR[@]}" models pull $MODELS -config "$CONFIG"

echo
"${NANOASR[@]}" models list -config "$CONFIG"
