#!/usr/bin/env bash
#
# Load test: what SPEC §15 asks for, against a running server.
#
#   100 concurrent five-minute files. The queue must not overflow, memory must
#   not grow, and RSS must be stable after thirty minutes.
#
# It drives the real HTTP surface rather than the pipeline directly, because
# most of what this test is looking for lives outside the pipeline: the upload
# limit, the spool's byte budget, the queue's admission control and the
# database. A Go benchmark over Transcribe would exercise none of them.
#
# It asserts nothing. The numbers depend entirely on the host, and a threshold
# here would fail on a laptop and pass on a server while telling nobody
# anything — the same reasoning as TestM1Report. What it does is print a table
# an operator can read, and make a leak obvious by showing RSS over time.
set -euo pipefail

URL="${URL:-http://127.0.0.1:8080}"
KEY="${KEY:-}"
CONCURRENCY="${CONCURRENCY:-100}"
DURATION_MIN="${DURATION_MIN:-30}"
CLIP_MIN="${CLIP_MIN:-5}"
SOURCE="${SOURCE:-testdata/audio/ru-16k.wav}"
WORK="${WORK:-$(mktemp -d)}"

log() { printf '==> %s\n' "$*" >&2; }

# curl and ffmpeg only: the other scripts here depend on nothing else, and the
# few numbers this needs out of the API are read with grep rather than by
# adding a JSON parser to the list of things an operator must install.
for tool in curl ffmpeg; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done
[[ -f "$SOURCE" ]] || { echo "no source clip at $SOURCE; run scripts/fetch-testdata.sh" >&2; exit 1; }

auth=()
[[ -n "$KEY" ]] && auth=(-H "Authorization: Bearer $KEY")

if ! curl -sf "${auth[@]}" "$URL/healthz" >/dev/null; then
  echo "no server at $URL" >&2
  exit 1
fi

# One clip, reused by every worker. Building it once matters: a hundred copies
# of five minutes of audio is 500 MB of disk for no benefit, and the server
# never sees the file name anyway.
CLIP="$WORK/clip-${CLIP_MIN}min.wav"
if [[ ! -f "$CLIP" ]]; then
  log "building a ${CLIP_MIN}-minute clip from $SOURCE"
  loops=$(( CLIP_MIN * 60 / 11 + 1 ))
  ffmpeg -hide_banner -loglevel error -y -stream_loop "$loops" -i "$SOURCE" \
    -t "$(( CLIP_MIN * 60 ))" -c:a pcm_s16le "$CLIP"
fi
log "clip is $(du -h "$CLIP" | cut -f1)"

# The server's own process, for RSS. Finding it by port keeps this honest when
# several builds are on the machine.
port="${URL##*:}"
port="${port%%/*}"
pid="$(ss -lptnH "sport = :$port" 2>/dev/null | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2 || true)"
if [[ -z "$pid" ]]; then
  log "could not find the server pid on port $port; RSS will not be reported"
fi

rss_mb() {
  [[ -z "$pid" ]] && { echo "-"; return; }
  awk '/VmRSS/ {printf "%d", $2/1024}' "/proc/$pid/status" 2>/dev/null || echo "-"
}

submitted=0
rejected=0
failed=0

# submit posts one job and prints its id, or a marker for a refusal. A 429 is
# not a failure — it is the queue doing its job — so the two are counted apart.
submit() {
  local code body
  body="$(curl -s -o "$WORK/resp.$$" -w '%{http_code}' \
    "${auth[@]}" -X POST "$URL/api/v1/jobs" -F "file=@$CLIP" || echo 000)"
  code="$body"
  case "$code" in
    202|200) grep -o '"id":"[^"]*"' < "$WORK/resp.$$" | head -1 | cut -d'"' -f4 ;;
    429)     echo "__rejected__" ;;
    *)       echo "__failed__:$code" ;;
  esac
  rm -f "$WORK/resp.$$"
}

log "load: $CONCURRENCY concurrent submissions for $DURATION_MIN minutes against $URL"
printf '%-8s %8s %10s %8s %8s %8s %8s\n' \
  "elapsed" "rss_mb" "submitted" "queued" "running" "rejected" "failed"

start=$SECONDS
deadline=$(( start + DURATION_MIN * 60 ))
next_report=$start

while (( SECONDS < deadline )); do
  # Top the queue back up to the target concurrency.
  ready="$(curl -sf "${auth[@]}" "$URL/readyz" || echo '{}')"
  depth="$(grep -o '"queue_depth":[0-9]*' <<<"$ready" | cut -d: -f2)"
  depth="${depth:-0}"

  want=$(( CONCURRENCY - depth ))
  (( want < 0 )) && want=0

  for _ in $(seq "$want"); do
    id="$(submit)"
    case "$id" in
      __rejected__)  rejected=$(( rejected + 1 )) ;;
      __failed__:*)  failed=$(( failed + 1 )) ;;
      *)             submitted=$(( submitted + 1 )) ;;
    esac
  done

  if (( SECONDS >= next_report )); then
    running="$(curl -sf "${auth[@]}" "$URL/api/v1/jobs?status=running" 2>/dev/null \
      | grep -o '"id":"job_' | wc -l || echo '-')"
    printf '%-8s %8s %10s %8s %8s %8s %8s\n' \
      "$(( (SECONDS - start) / 60 ))m" "$(rss_mb)" "$submitted" "$depth" "$running" "$rejected" "$failed"
    next_report=$(( SECONDS + 60 ))
  fi
  sleep 5
done

log "done after $(( (SECONDS - start) / 60 )) minutes"
log "submitted=$submitted rejected(429)=$rejected failed=$failed final_rss_mb=$(rss_mb)"
log ""
log "What to look for:"
log "  - rss_mb flat after the first few minutes. A rising line is a leak."
log "  - failed=0. A 429 is the queue refusing work it cannot hold, which is"
log "    correct; anything else is not."
log "  - queued never above jobs.queue_size."
