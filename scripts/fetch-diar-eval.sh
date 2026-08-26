#!/usr/bin/env bash
#
# Build a Russian diarization benchmark from the Dialogs corpus.
#
#   https://huggingface.co/datasets/langswap/dialogs-ru-emotional-conversations
#   20.6 hours of studio dialogue between real speakers, OpenRAIL licensed,
#   cut into one WAV per utterance with a speaker id beside each.
#
# That last property is the point. Diarization can only be measured against a
# reference, and hand-labelling one is a day of work; here the labels already
# exist, so concatenating the utterances of a dialog in order yields audio whose
# every turn boundary is known exactly. Nothing is estimated.
#
# What this produces:
#   testdata/audio/diar-<part>-<n>.wav   16 kHz mono, the concatenated dialog
#   testdata/audio/diar-<part>-<n>.rttm  the reference, in the standard format
#
# Only a slice is fetched: fifteen minutes exercises a hundred turns and runs
# through the pipeline in under a minute, which is what makes it useful while
# tuning rather than once a week.
set -euo pipefail

PART="${PART:-masha_dima_part1}"
UTTERANCES="${UTTERANCES:-115}"
# Silence inserted between turns. The corpus stores utterances separately and
# does not record the pauses between them, so this is chosen rather than
# recovered: 300 ms is about the average inter-turn gap in conversation. It is
# also deliberately below diarization.min_duration_off, so a configuration that
# merges turns across a natural pause shows up as error instead of hiding.
GAP="${GAP:-0.3}"
# Silence detection, used to make the reference describe speech rather than
# files. -45 dB is well below conversational level and above the noise floor of
# a studio recording; 200 ms is short enough to catch a pause between clauses
# and long enough not to split a word at its stop consonant.
SILENCE_DB="${SILENCE_DB:--45dB}"
SILENCE_MIN="${SILENCE_MIN:-0.2}"
# Speech shorter than this is not worth a reference line; it is an artefact of
# the detector rather than a turn.
SPEECH_MIN="${SPEECH_MIN:-0.15}"
OUT="${OUT:-testdata/audio}"
WORK="${WORK:-.cache/dialogs}"

REPO="https://huggingface.co/datasets/langswap/dialogs-ru-emotional-conversations/resolve/main"
NAME="diar-${PART}-${UTTERANCES}"

for tool in curl ffmpeg ffprobe awk; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done

mkdir -p "$WORK/wavs" "$OUT"

if [[ ! -f "$WORK/metadata.csv" ]]; then
  echo "==> fetching the corpus index" >&2
  curl -fsSL "$REPO/metadata.csv" -o "$WORK/metadata.csv"
fi

# The index is pipe-separated: audio_path|speaker_id|text|emotion|...|duration.
# Utterances are numbered in the file name, and lexical order would put _10
# before _2, so the number is sorted on as a number.
awk -F'|' -v part="$PART" -v n="$UTTERANCES" '
  NR == 1 { next }
  index($1, "wavs/" part "_") == 1 {
    idx = $1; sub(/^.*_/, "", idx); sub(/\.wav$/, "", idx)
    print idx "\t" $1 "\t" $2
  }
' "$WORK/metadata.csv" | sort -n -k1,1 | cut -f2,3 > "$WORK/$NAME.candidates"

candidates=$(wc -l < "$WORK/$NAME.candidates")
[[ "$candidates" -gt 0 ]] || { echo "no utterances matched part $PART" >&2; exit 1; }

# Twice the target, because the published index runs ahead of the published
# audio: a fifth of part1's first hundred utterances have a row and no file.
# Fetching a surplus and filtering afterwards is what keeps the slice the size
# it was asked for.
head -n "$((UTTERANCES * 2))" "$WORK/$NAME.candidates" > "$WORK/$NAME.wanted"
echo "==> $PART: $candidates utterances indexed, taking $UTTERANCES" >&2

echo "==> downloading" >&2
# curl's --config form fetches every missing file over one connection instead of
# paying a TLS handshake per utterance. The config is written directly rather
# than rewritten out of a command line, because a path is not a thing to parse.
: > "$WORK/$NAME.cfg"
missing=0
while IFS=$'\t' read -r path _; do
  dst="$WORK/$path"
  [[ -f "$dst" ]] && continue
  mkdir -p "$(dirname "$dst")"
  printf 'url = "%s/%s"\noutput = "%s"\n' "$REPO" "$path" "$dst" >> "$WORK/$NAME.cfg"
  missing=$((missing + 1))
done < "$WORK/$NAME.wanted"

if (( missing > 0 )); then
  echo "    $missing files" >&2
  # Not -f alone: an utterance the index names but the repository does not hold
  # answers 404, and curl must carry on to the rest rather than stop at it.
  curl -fsSL --config "$WORK/$NAME.cfg" || true
fi

# The reference is built from audio that exists, so the missing utterances are
# dropped here rather than papered over later.
: > "$WORK/$NAME.list"
absent=0
while IFS=$'\t' read -r path speaker; do
  if [[ -f "$WORK/$path" ]]; then
    printf '%s\t%s\n' "$path" "$speaker" >> "$WORK/$NAME.list"
  else
    absent=$((absent + 1))
  fi
done < "$WORK/$NAME.wanted"

head -n "$UTTERANCES" "$WORK/$NAME.list" > "$WORK/$NAME.list.tmp"
mv "$WORK/$NAME.list.tmp" "$WORK/$NAME.list"
count=$(wc -l < "$WORK/$NAME.list")
if (( absent > 0 )); then
  echo "    $absent indexed utterances have no audio upstream and were skipped" >&2
fi
[[ "$count" -gt 0 ]] || { echo "no audio was retrieved for $PART" >&2; exit 1; }

# Silence between turns, in the target format so the concat demuxer has one
# uniform input.
SILENCE="$WORK/silence-${GAP}.wav"
[[ -f "$SILENCE" ]] || ffmpeg -nostdin -hide_banner -loglevel error -y \
  -f lavfi -i "anullsrc=r=16000:cl=mono" -t "$GAP" -c:a pcm_s16le "$SILENCE"

echo "==> converting and measuring" >&2
: > "$WORK/$NAME.concat"
: > "$OUT/$NAME.rttm"

# Each utterance is resampled before being measured, so the reference describes
# the audio that is actually produced rather than the audio it came from.
start=0
while IFS=$'\t' read -r path speaker; do
  src="$WORK/$path"
  dst="${src%.wav}.16k.wav"
  [[ -f "$dst" ]] || ffmpeg -nostdin -hide_banner -loglevel error -y -i "$src" \
    -ac 1 -ar 16000 -c:a pcm_s16le "$dst"

  dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$dst" < /dev/null)
  printf "file '%s'\n" "$(cd "$(dirname "$dst")" && pwd)/$(basename "$dst")" >> "$WORK/$NAME.concat"
  printf "file '%s'\n" "$(cd "$(dirname "$SILENCE")" && pwd)/$(basename "$SILENCE")" >> "$WORK/$NAME.concat"

  # The reference marks speech, not files. A third to a half of each utterance
  # is silence — padding at the edges and pauses inside the sentence — and
  # calling all of it speech charges every system with a missed-speech error it
  # did not make. Detected once per utterance and cached, because this is the
  # slow part of the build.
  sil="${src%.wav}.silence"
  # -v info, not error: silencedetect reports on the info channel, and
  # silencing ffmpeg silences the detector along with it.
  [[ -f "$sil" ]] || ffmpeg -nostdin -hide_banner -nostats -v info -i "$dst" \
    -af "silencedetect=noise=${SILENCE_DB}:d=${SILENCE_MIN}" -f null - 2>&1 |
    awk '/silence_start:/ { print "s", $NF } /silence_end:/ { print "e", $5 }' > "$sil"

  # Speech is what is left between the silences.
  awk -v f="$NAME" -v off="$start" -v d="$dur" -v spk="$speaker" -v minlen="$SPEECH_MIN" '
    $1 == "s" { starts[++ns] = $2 + 0 }
    $1 == "e" { ends[++ne] = $2 + 0 }
    END {
      pos = 0
      for (i = 1; i <= ns; i++) {
        if (starts[i] - pos >= minlen)
          printf "SPEAKER %s 1 %.3f %.3f <NA> <NA> %s <NA> <NA>\n", f, off + pos, starts[i] - pos, spk
        pos = (i <= ne) ? ends[i] : d
      }
      if (d - pos >= minlen)
        printf "SPEAKER %s 1 %.3f %.3f <NA> <NA> %s <NA> <NA>\n", f, off + pos, d - pos, spk
    }
  ' "$sil" >> "$OUT/$NAME.rttm"

  start=$(awk -v s="$start" -v d="$dur" -v g="$GAP" 'BEGIN { printf "%.6f", s + d + g }')
done < "$WORK/$NAME.list"

echo "==> concatenating" >&2
ffmpeg -nostdin -hide_banner -loglevel error -y -f concat -safe 0 -i "$WORK/$NAME.concat" \
  -c:a pcm_s16le "$OUT/$NAME.wav"

total=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$OUT/$NAME.wav" < /dev/null)
speakers=$(cut -f2 "$WORK/$NAME.list" | sort -u | tr '\n' ' ')
speech=$(awk '{ t += $5 } END { printf "%.1f", t }' "$OUT/$NAME.rttm")
lines=$(wc -l < "$OUT/$NAME.rttm")
printf '\n%s\n  %s\n    %.1f min audio, %s speech, %s turns, %s reference regions\n    speakers: %s\n  %s\n' \
  "built" "$OUT/$NAME.wav" "$(awk -v t="$total" 'BEGIN{print t/60}')" \
  "$(awk -v s="$speech" 'BEGIN{printf "%.1f min", s/60}')" \
  "$count" "$lines" "$speakers" "$OUT/$NAME.rttm"
