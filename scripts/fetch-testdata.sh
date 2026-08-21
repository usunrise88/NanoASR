#!/usr/bin/env bash
#
# Fetch and derive the audio the integration tests use.
#
# Audio is not committed: it has its own licensing, it bloats the repository,
# and every derived form is reproducible from one source file. What IS
# committed is the expected transcript in testdata/golden.
set -euo pipefail

OUT="${OUT:-testdata/audio}"
SOURCE_URL="https://huggingface.co/csukuangfj/tmp-files/resolve/main/GigaAM/example.wav"
SOURCE_SHA="d8aaaa18a5098d7c6de0595ae7ac1e64cacd0d4022af3595213bdaf23be77e69"

log() { printf '==> %s\n' "$*" >&2; }

if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "ffmpeg is required to derive the telephony variants" >&2
  exit 1
fi

mkdir -p "$OUT"

if [[ ! -f "$OUT/ru-16k.wav" ]]; then
  log "downloading the reference clip"
  curl -fSL --retry 3 -o "$OUT/ru-16k.wav" "$SOURCE_URL"
fi

# The checksum is informational: the upstream file is not under our control, so
# a mismatch is a warning to re-check the golden transcript, not a hard failure.
got="$(sha256sum "$OUT/ru-16k.wav" | cut -d' ' -f1)"
if [[ "$got" != "$SOURCE_SHA" ]]; then
  echo "note: reference clip checksum is $got (expected $SOURCE_SHA)" >&2
  echo "      if the upstream file changed, regenerate testdata/golden" >&2
fi

# Telephony variants. These are the shapes the server actually receives: 8 kHz
# narrowband, companded, mono. The comparison between ru-16k and ru-8k-alaw is
# what the M1 report measures.
log "deriving telephony and compressed variants"
ffmpeg -hide_banner -loglevel error -y -i "$OUT/ru-16k.wav" \
  -ar 8000 -ac 1 -c:a pcm_alaw "$OUT/ru-8k-alaw.wav"
ffmpeg -hide_banner -loglevel error -y -i "$OUT/ru-16k.wav" \
  -ar 8000 -ac 1 -c:a pcm_mulaw "$OUT/ru-8k-ulaw.wav"
ffmpeg -hide_banner -loglevel error -y -i "$OUT/ru-16k.wav" \
  -c:a libmp3lame -b:a 64k "$OUT/ru-16k.mp3"
ffmpeg -hide_banner -loglevel error -y -i "$OUT/ru-16k.wav" \
  -c:a libopus -b:a 24k "$OUT/ru-16k.opus"

log "test audio in $OUT:"
ls -1 "$OUT"
