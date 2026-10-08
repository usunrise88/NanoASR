#!/usr/bin/env bash
#
# Install NanoASR on Linux as a systemd service.
#
#   curl -fsSL https://github.com/usunrise88/NanoASR/releases/latest/download/install.sh | bash
#
# The one-liner is the whole interface: it works out the latest release, checks
# that this machine can run it, unpacks the archive under /opt/nanoasr, writes a
# configuration with two API keys, downloads the models and leaves a running
# service behind. Every decision it makes is printed, and every one can be
# overridden — by a flag, or by the matching NANOASR_* variable for the pipe
# where flags are awkward:
#
#   curl -fsSL .../install.sh | bash -s -- --addr 0.0.0.0:8080 --no-download
#   NANOASR_ADDR=0.0.0.0:8080 curl -fsSL .../install.sh | bash
#
# Run it as root or as a user who can sudo; --help lists the rest.
set -euo pipefail

REPO="${NANOASR_REPO:-usunrise88/NanoASR}"
VERSION="${NANOASR_VERSION:-}"
PREFIX="${NANOASR_PREFIX:-/opt/nanoasr}"
DATA_DIR="${NANOASR_DATA_DIR:-/var/lib/nanoasr}"
ADDR="${NANOASR_ADDR:-127.0.0.1:8080}"
# Whether an address was asked for at all, which is a different question
# from what it is: an upgrade keeps the configuration, so a flag that was
# given and not applied has to be reported rather than silently dropped.
ADDR_SET=0; [[ -n "${NANOASR_ADDR:-}" ]] && ADDR_SET=1
SVC_USER="${NANOASR_USER:-nanoasr}"
SERVICE="${NANOASR_SERVICE:-nanoasr}"
WITH_UI="${NANOASR_UI:-1}"
WITH_MODELS="${NANOASR_DOWNLOAD:-1}"
WITH_FFMPEG="${NANOASR_FFMPEG:-1}"
REALTIME="${NANOASR_REALTIME:-0}"
# Empty means "leave it as it is": the CPU on a fresh install, and whatever an
# existing one is already using on an upgrade. 1 is the GPU, 0 is back to the
# CPU. Not a default of 0, because that would quietly undo somebody's GPU
# installation on the next upgrade.
GPU="${NANOASR_GPU:-}"
CUDA="${NANOASR_CUDA:-12}"
START="${NANOASR_START:-1}"
ACTION=install
PURGE=0
# The piped form cannot pass a flag either, so these two have variables as well.
if [[ "${NANOASR_UNINSTALL:-}" == "1" ]]; then ACTION=uninstall; fi
if [[ "${NANOASR_PURGE:-}" == "1" ]]; then PURGE=1; fi

UNIT_DIR=/etc/systemd/system
SUDO=""
TMP=""
BASE=""

say()  { printf '==> %s\n' "$*" >&2; }
warn() { printf '    warning: %s\n' "$*" >&2; }
die()  { printf 'install.sh: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# systemd_running distinguishes "systemctl is installed" from "systemd is PID 1",
# which a container image often gets to disagree about. Every systemctl call is
# behind it, because one against a machine that is not running systemd fails
# with "Failed to connect to bus" and would otherwise abort the script halfway.
systemd_running() { have systemctl && [[ -d /run/systemd/system ]]; }

usage() {
  cat >&2 <<EOF
Install NanoASR as a systemd service.

  --version TAG     release to install                   (default: the latest)
  --prefix DIR      binary, libraries and configuration  ($PREFIX)
  --data-dir DIR    models, job database and spool       ($DATA_DIR)
  --addr HOST:PORT  listen address                       ($ADDR)
  --user NAME       system account to run as             ($SVC_USER)
  --service NAME    systemd unit name                    ($SERVICE)
  --no-ui           install the build without the web interface
  --no-download     write the configuration but fetch no models
  --no-ffmpeg       do not install ffmpeg
  --realtime        also serve streaming recognition over a websocket,
                    and fetch the Russian streaming model it needs
  --gpu             recognise on an NVIDIA GPU: install the CUDA build of the
                    native libraries and point the service at them. Works on a
                    fresh install and on one already running on the CPU.
  --no-gpu          put an installation that is using the GPU back on the CPU
  --cuda 12|13      CUDA major version for --gpu             ($CUDA)
  --no-start        install everything, start nothing
  --uninstall       stop and remove the service and the installation
  --purge           with --uninstall: also delete the data and the account

Each option has an environment variable: NANOASR_VERSION, NANOASR_PREFIX,
NANOASR_DATA_DIR, NANOASR_ADDR, NANOASR_USER, NANOASR_SERVICE, NANOASR_UI=0,
NANOASR_DOWNLOAD=0, NANOASR_FFMPEG=0, NANOASR_REALTIME=1, NANOASR_GPU=1 (or 0),
NANOASR_CUDA, NANOASR_START=0, NANOASR_UNINSTALL=1, NANOASR_PURGE=1. A flag
wins over one.

The CPU is the default and an upgrade never changes which one you are on: an
installation already using the GPU keeps it, with its libraries refreshed to
match the new binary. install-gpu.sh is the same step on its own, with more
options.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)     VERSION="${2:?--version needs a tag}"; shift 2 ;;
    --prefix)      PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
    --data-dir)    DATA_DIR="${2:?--data-dir needs a directory}"; shift 2 ;;
    --addr)        ADDR="${2:?--addr needs host:port}"; ADDR_SET=1; shift 2 ;;
    --user)        SVC_USER="${2:?--user needs a name}"; shift 2 ;;
    --service)     SERVICE="${2:?--service needs a name}"; shift 2 ;;
    --no-ui)       WITH_UI=0; shift ;;
    --no-download) WITH_MODELS=0; shift ;;
    --no-ffmpeg)   WITH_FFMPEG=0; shift ;;
    --realtime)    REALTIME=1; shift ;;
    --gpu)         GPU=1; shift ;;
    --no-gpu)      GPU=0; shift ;;
    --cuda)        CUDA="${2:?--cuda needs 12 or 13}"; shift 2 ;;
    --no-start)    START=0; shift ;;
    --uninstall)   ACTION=uninstall; shift ;;
    --purge)       PURGE=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

CONFIG="$PREFIX/nanoasr.yaml"
UNIT="$UNIT_DIR/$SERVICE.service"

cleanup() {
  [[ -n "$TMP" ]] && rm -rf "$TMP"
  return 0
}
trap cleanup EXIT

# --- privileges and preconditions --------------------------------------------

# escalate decides how the privileged steps run.
#
# Root is needed for /opt, /etc/systemd and useradd, and for nothing else, so
# each privileged command is prefixed rather than the script re-executing
# itself as root: there is nothing to re-execute when the script arrived
# through a pipe, and sudo reads the password from the terminal even when
# standard input is that pipe.
escalate() {
  if [[ "$(id -u)" -eq 0 ]]; then
    SUDO=""
    return
  fi
  have sudo || die "this needs root and sudo is not installed; run it as root"
  SUDO="sudo"
  say "not running as root — the privileged steps go through sudo"
  sudo -v || die "sudo refused; run this as root instead"
}

# as_service_user runs a command as the account the service will use, so that
# what it writes — the database, the models, the spool — already belongs to the
# account that has to write to them later.
as_service_user() {
  if have sudo; then
    sudo -u "$SVC_USER" -- "$@"
  elif have runuser; then
    runuser -u "$SVC_USER" -- "$@"
  else
    die "neither sudo nor runuser is here, so nothing can run as $SVC_USER"
  fi
}

preflight() {
  [[ "$(uname -s)" == "Linux" ]] || die "this installs a systemd service; on Windows use install.ps1"
  case "$(uname -m)" in
    x86_64|amd64) ;;
    *) die "the published builds are x86-64 only and this is $(uname -m); build from source instead" ;;
  esac
  # The release is linked against glibc, and what a musl system says about it is
  # "not found" for a binary that is plainly there.
  if compgen -G "/lib/ld-musl-*" >/dev/null 2>&1; then
    die "this is a musl system (Alpine); the release needs glibc — use the Docker image or build from source"
  fi
  for tool in curl tar sha256sum; do
    have "$tool" || die "$tool is required"
  done
  if ! systemd_running; then
    warn "systemd is not running here; the files will be installed and no service will be"
    START=0
  fi
}

# --- the release --------------------------------------------------------------

# latest_tag asks GitHub which release is current.
#
# The redirect that /releases/latest performs is asked first because it is not
# rate limited, which the API very much is on a shared address.
latest_tag() {
  local url tag
  url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" 2>/dev/null || true)"
  tag="${url##*/}"
  if [[ "$url" == *"/releases/tag/"* && -n "$tag" ]]; then
    printf '%s\n' "$tag"
    return
  fi
  tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [[ -n "$tag" ]] || die "cannot work out the latest release; pass --version TAG"
  printf '%s\n' "$tag"
}

# A hundred megabytes with no output looks like a hang, so the meter stays on;
# -# keeps it to one line when this is a terminal and to a few when it is not.
fetch() { curl -fSL -# --retry 4 --retry-delay 2 --connect-timeout 20 -o "$2" "$1"; }

# verify checks the archive against the published checksums. HTTPS already says
# the bytes came from GitHub; this says that all of them arrived. A release from
# before the checksums were published is not a reason to refuse.
verify() {
  local dir="$1" file="$2" line pattern
  if ! fetch "$BASE/sha256sums.txt" "$dir/sha256sums.txt" 2>/dev/null; then
    warn "$VERSION publishes no sha256sums.txt, so $file was not verified"
    return
  fi
  # The name is matched literally: it is full of dots, and a dot in a pattern
  # matches anything, which is not what a checksum lookup may do.
  pattern="$(printf '%s' "$file" | sed 's/[^[:alnum:]_-]/\\&/g')"
  line="$(grep -E "[[:space:]][*]?${pattern}\$" "$dir/sha256sums.txt" || true)"
  [[ -n "$line" ]] || die "sha256sums.txt does not mention $file"
  (cd "$dir" && printf '%s\n' "$line" | sha256sum -c --quiet -) || die "$file failed its checksum"
  say "checksum ok"
}

# --- installation -------------------------------------------------------------

ensure_user() {
  id -u "$SVC_USER" >/dev/null 2>&1 && return 0
  local shell=/usr/sbin/nologin
  [[ -x "$shell" ]] || shell=/sbin/nologin
  [[ -x "$shell" ]] || shell=/bin/false
  say "creating the system account $SVC_USER"
  $SUDO useradd --system --home-dir "$DATA_DIR" --shell "$shell" "$SVC_USER" ||
    die "could not create the account $SVC_USER"
}

# ensure_ffmpeg installs ffmpeg when the distribution has an obvious way to.
#
# Without it NanoASR accepts WAV and raw PCM and answers 415 to everything else,
# which is a surprising way to discover that a package is missing. Failing here
# is not fatal: it is a convenience, not a dependency.
ensure_ffmpeg() {
  [[ "$WITH_FFMPEG" == "1" ]] || return 0
  have ffmpeg && return 0
  say "installing ffmpeg (mp3, m4a, ogg and the rest need it)"
  if have apt-get; then
    $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq &&
      $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends ffmpeg
  elif have dnf;    then $SUDO dnf install -y -q ffmpeg-free || $SUDO dnf install -y -q ffmpeg
  elif have yum;    then $SUDO yum install -y -q ffmpeg
  elif have zypper; then $SUDO zypper --non-interactive install ffmpeg
  elif have pacman; then $SUDO pacman -Sy --noconfirm ffmpeg
  else
    warn "no package manager recognised; install ffmpeg for anything but WAV"
    return 0
  fi || warn "ffmpeg could not be installed; WAV and raw PCM still work"
}

# config_has_keys reports whether the configuration is a working one rather than
# the keyless example the archive ships. It is what decides between writing a
# configuration and keeping the one that is already there, and it is asked of
# the file rather than of the presence of a unit, because an installation done
# by hand has keys and no unit.
config_has_keys() {
  [[ -f "$CONFIG" ]] || return 1
  local out
  # Through $SUDO because the configuration holds API keys and is written 0600
  # to the service account: an unprivileged read fails, and a failed read that
  # was taken to mean "no keys" would overwrite a working configuration with a
  # fresh one on every upgrade.
  out="$($SUDO "$PREFIX/nanoasr" key list -config "$CONFIG" 2>/dev/null)" || return 1
  [[ "$out" != *"no keys in"* ]]
}

# config_value reads one two-space-indented key out of the configuration.
#
# A kept configuration is the only place the truth about an upgrade lives: the
# flags say what was asked for, the file says what the server will do, and on
# an upgrade those are not the same thing. Through $SUDO because the file is
# 0600 to the service account; awk rather than a pipe to head so there is no
# SIGPIPE to trip over under pipefail.
config_value() {
  local key="$1" out
  [[ -f "$CONFIG" ]] || return 1
  out="$($SUDO awk -v k="$key" '
    index($0, "  " k ":") == 1 { sub("^  " k ":[ \t]*", ""); sub("#.*", ""); print; exit }
  ' "$CONFIG" 2>/dev/null)" || return 1
  out="$(printf '%s' "$out" | tr -d "\"' ")"
  [[ -n "$out" ]] || return 1
  printf '%s' "$out"
}

# config_has_realtime reads api.dialects, in either shape YAML allows:
#
#   dialects: [openai, native, realtime]
#   dialects:
#     - openai
#     - realtime
#
# config_value reads a value that sits on the key's own line, which the second
# shape does not have. Getting this wrong is not harmless: the upgrade path
# tells the operator to add a dialect that is already there, and the closing
# banner hides a websocket endpoint the server is serving.
config_has_realtime() {
  [[ -f "$CONFIG" ]] || return 1
  $SUDO awk '
    index($0, "  dialects:") == 1 {
      line = $0
      sub("#.*", "", line)
      if (line ~ /realtime/) { found = 1; exit }
      block = 1
      next
    }
    block {
      line = $0
      sub("#.*", "", line)
      # The list ends at the first line that is not one of its items.
      if (line !~ /^[ \t]*-/) {
        if (line ~ /[^ \t]/) exit
        next
      }
      if (line ~ /realtime/) { found = 1; exit }
    }
    END { exit found ? 0 : 1 }
  ' "$CONFIG" 2>/dev/null
}

# effective_addr is what the server will actually listen on: the
# configuration's value when there is one, and what was asked for when there is
# not. On a fresh install the two agree, because init was handed $ADDR.
effective_addr() {
  local addr
  if addr="$(config_value addr)"; then
    printf '%s' "$addr"
  else
    printf '%s' "$ADDR"
  fi
}

# flags_not_applied names the init-only flags an upgrade swallowed.
#
# Keeping the configuration is right — rewriting a file somebody else owns is
# not an upgrade's business — but a flag that reaches only `nanoasr init` then
# does nothing, and saying nothing about it is how somebody comes to believe
# the server moved to an address it never moved to.
flags_not_applied() {
  if [[ "$ADDR_SET" == "1" ]]; then
    local addr; addr="$(effective_addr)"
    if [[ "$addr" != "$ADDR" ]]; then
      warn "--addr was not applied: an upgrade keeps the configuration.
    $SERVICE listens on $addr. To move it, set server.addr in $CONFIG
    and restart, or override it for the service alone:
      systemctl edit $SERVICE   ->   [Service] Environment=NANOASR_ADDR=$ADDR"
    fi
  fi
  if [[ "$REALTIME" == "1" ]] && ! config_has_realtime; then
    warn "--realtime was not applied: an upgrade keeps the configuration.
    Add realtime to api.dialects in $CONFIG, then fetch the streaming model
    and restart:
      sudo -u $SVC_USER $PREFIX/nanoasr models pull -configured -config $CONFIG
      systemctl restart $SERVICE"
  fi
}

# write_unit derives the unit from the one in the archive, so that the hardening
# it carries stays defined in one place and only the paths move.
write_unit() {
  local src="$PREFIX/nanoasr.service" field
  [[ -f "$src" ]] || die "the archive did not contain nanoasr.service"
  for field in User Group WorkingDirectory ExecStart ReadWritePaths; do
    grep -q "^$field=" "$src" || die "$src has no $field= line to point at $PREFIX"
  done
  say "writing $UNIT"
  sed -e "s#^User=.*#User=$SVC_USER#" \
      -e "s#^Group=.*#Group=$SVC_USER#" \
      -e "s#^WorkingDirectory=.*#WorkingDirectory=$PREFIX#" \
      -e "s#^ExecStart=.*#ExecStart=$PREFIX/nanoasr serve -config $CONFIG#" \
      -e "s#^ReadWritePaths=.*#ReadWritePaths=$DATA_DIR#" \
      "$src" | $SUDO tee "$UNIT" >/dev/null
  $SUDO systemctl daemon-reload
}

# probe_url turns the listen address into one curl can ask: a server bound to
# every interface is reached over the loopback like any other.
#
# The address comes from the configuration rather than from $ADDR, because on
# an upgrade those differ and the one worth printing is the one the server will
# use. Advertising a URL nobody is listening on is worse than printing none.
probe_url() {
  local addr; addr="$(effective_addr)"
  local host="${addr%:*}" port="${addr##*:}"
  case "$host" in
    ""|"0.0.0.0"|"::"|"[::]"|"*") host=127.0.0.1 ;;
  esac
  printf 'http://%s:%s' "$host" "$port"
}

install_nanoasr() {
  escalate
  preflight

  [[ -n "$VERSION" ]] || { say "asking GitHub for the latest release"; VERSION="$(latest_tag)"; }
  local suffix="" archive keys=""
  [[ "$WITH_UI" == "1" ]] || suffix="-noui"
  archive="nanoasr-${VERSION}-linux-amd64${suffix}.tar.gz"
  BASE="https://github.com/$REPO/releases/download/$VERSION"

  TMP="$(mktemp -d)"
  say "downloading $archive"
  fetch "$BASE/$archive" "$TMP/$archive" || die "no such release asset: $BASE/$archive"
  verify "$TMP" "$archive"

  # Stopped before anything is overwritten: replacing the binary and the shared
  # libraries under a running process is how a restart ends up mixing versions.
  if systemd_running && [[ -f "$UNIT" ]]; then
    say "stopping the running $SERVICE service"
    $SUDO systemctl stop "$SERVICE" 2>/dev/null || true
  fi

  ensure_user
  $SUDO install -d -o "$SVC_USER" -g "$SVC_USER" -m 0755 "$PREFIX" "$DATA_DIR"

  # The archive ships its own nanoasr.yaml, which on an upgrade is the working
  # configuration, keys and all. So it is the one file not unpacked.
  say "unpacking into $PREFIX"
  if config_has_keys; then
    $SUDO tar -xzf "$TMP/$archive" -C "$PREFIX" --exclude=./nanoasr.yaml
  else
    $SUDO tar -xzf "$TMP/$archive" -C "$PREFIX"
  fi
  $SUDO chown -R "$SVC_USER:$SVC_USER" "$PREFIX" "$DATA_DIR"

  ensure_ffmpeg

  if config_has_keys; then
    say "keeping the configuration in $CONFIG"
    # A release can change a default model — 1.0.5 moved diarization to
    # nemotron-3-diarization — and a server that fetches it on the first
    # request that needs it makes that request wait for the download.
    if [[ "$WITH_MODELS" == "1" ]]; then
      say "fetching any model the configuration uses that is not installed yet"
      as_service_user "$PREFIX/nanoasr" models pull -configured -config "$CONFIG" ||
        warn "could not fetch every configured model; the server will fetch what is missing when first needed"
    fi
    flags_not_applied
  else
    say "writing the configuration and fetching the models (this is gigabytes)"
    local init_args=(init -config "$CONFIG" -data-dir "$DATA_DIR" -addr "$ADDR" -force)
    [[ "$WITH_MODELS" == "1" ]] || init_args+=(-no-download)
    # Only on a fresh configuration: adding the dialect to an existing one
    # would mean editing a file somebody else wrote, and the streaming model is
    # a separate 128 MB download either way.
    #
    # An if rather than `[[ ... ]] && ...`: under set -e the && form exits the
    # script when the condition is false, because the statement's status is
    # then 1 and nothing is consuming it.
    if [[ "$REALTIME" == "1" ]]; then
      init_args+=(-realtime)
    fi
    as_service_user "$PREFIX/nanoasr" "${init_args[@]}" 2>&1 | tee "$TMP/init.log" ||
      die "nanoasr init did not finish — the output above says why. Nothing else was changed; run this again once it is fixed"
    keys="$(grep -E '^(admin|user) key' "$TMP/init.log" || true)"
  fi

  if ! systemd_running; then
    apply_gpu_choice
    say "installed in $PREFIX; start it with: $PREFIX/nanoasr serve -config $CONFIG"
    return
  fi

  write_unit
  # Before the service is started, so it comes up on the right libraries once
  # rather than on the CPU and then again a second later.
  apply_gpu_choice
  if [[ "$START" == "1" ]]; then
    say "enabling and starting $SERVICE"
    $SUDO systemctl enable --now "$SERVICE"
    wait_for_health
  else
    $SUDO systemctl enable "$SERVICE"
    say "not started (--no-start); start it with: sudo systemctl start $SERVICE"
  fi

  summary "$keys"
}

# --- the GPU libraries -------------------------------------------------------

# on_gpu reports whether this installation is using the CUDA libraries.
on_gpu() { [[ -d "$PREFIX/lib-gpu" ]]; }

# gpu_script finds install-gpu.sh, which does the work.
#
# Beside this script when it was run from a checkout or an unpacked release;
# otherwise from the same release this installed, never "latest" — the
# libraries have to match the binary, and a newer script may pin a newer
# archive.
gpu_script() {
  local here sibling
  here="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)"
  sibling="$here/install-gpu.sh"
  if [[ -n "$here" && -f "$sibling" ]]; then
    printf '%s' "$sibling"
    return
  fi
  [[ -n "$TMP" ]] || TMP="$(mktemp -d)"
  if ! fetch "$BASE/install-gpu.sh" "$TMP/install-gpu.sh"; then
    warn "this release does not publish install-gpu.sh ($BASE/install-gpu.sh);"
    warn "install the GPU libraries from a checkout, or from a release that has it"
    return 1
  fi
  printf '%s' "$TMP/install-gpu.sh"
}

# apply_gpu_choice installs, refreshes or removes the CUDA libraries.
#
# The third case is the one worth having: a plain upgrade of an installation
# that is using the GPU re-runs the install, because those libraries carry the
# sherpa-onnx version the binary was linked against and the new binary may need
# a different one.
apply_gpu_choice() {
  local args=(--prefix "$PREFIX" --service "$SERVICE" --cuda "$CUDA" --no-restart)

  case "$GPU" in
    1)
      say "installing the CUDA build of the native libraries"
      ;;
    0)
      if ! on_gpu; then
        return
      fi
      say "putting $SERVICE back on the CPU libraries"
      args+=(--revert)
      ;;
    *)
      if ! on_gpu; then
        return
      fi
      say "refreshing the GPU libraries to match the new binary"
      ;;
  esac

  # Not fatal: the script below says what went wrong and whether it changed
  # anything, and a failed GPU step leaves the CPU libraries in place — so the
  # rest of the installation, which is fine, is worth finishing. It does decide
  # the exit code, in main: a deployment that asked for the GPU and did not get
  # it has to be able to tell.
  local script
  if ! script="$(gpu_script)"; then
    GPU_FAILED=1
    return
  fi
  if ! bash "$script" "${args[@]}"; then
    warn "the GPU step did not finish; $SERVICE will run on the CPU"
    GPU_FAILED=1
  fi
}

# wait_for_health gives the server a minute to answer and reports rather than
# fails if it does not: the service exists either way, and the journal is where
# the reason would be.
wait_for_health() {
  local url i
  url="$(probe_url)"
  for ((i = 0; i < 60; i++)); do
    if curl -fsS --max-time 2 "$url/healthz" >/dev/null 2>&1; then
      say "$SERVICE answers on $url"
      return
    fi
    if ! $SUDO systemctl is-active --quiet "$SERVICE"; then
      warn "the service stopped; the reason is in: journalctl -u $SERVICE -n 50"
      return
    fi
    sleep 1
  done
  warn "no answer from $url/healthz yet — loading the weights takes a while: journalctl -u $SERVICE -f"
}

# provider_line is what the summary says about CPU or GPU, and it is read from
# the installation rather than from the flags: a GPU step that failed leaves
# the service on the CPU, and the summary has to say the same thing the server
# will.
provider_line() {
  if [[ "${GPU_FAILED:-0}" == "1" ]]; then
    printf 'cpu (the GPU step failed; see above)'
  elif on_gpu; then
    printf 'cuda    %s/lib-gpu, drop-in %s' "$PREFIX" "$UNIT_DIR/$SERVICE.service.d/10-gpu.conf"
  else
    printf 'cpu     --gpu installs the CUDA libraries'
  fi
}

summary() {
  local keys="$1" url
  url="$(probe_url)"
  cat >&2 <<EOF

NanoASR $VERSION is installed.

  service   $SERVICE          systemctl status $SERVICE
  provider  $(provider_line)
  binary    $PREFIX/nanoasr
  config    $CONFIG
  data      $DATA_DIR
  log       journalctl -u $SERVICE -f
  url       $url    ($url/ui in a browser)

EOF
  if [[ -n "$keys" ]]; then
    printf '%s\n\n' "$keys" >&2
  else
    printf '  The API keys are in %s (nanoasr key list -config %s).\n\n' "$CONFIG" "$CONFIG" >&2
  fi
  cat >&2 <<EOF
  curl -sS $url/v1/audio/transcriptions \\
    -H "Authorization: Bearer <the user key>" \\
    -F file=@audio.wav

  docs       $url/docs    (no key needed)

  update     curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | bash
  uninstall  curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | bash -s -- --uninstall
  gpu        curl -fsSL https://github.com/$REPO/releases/latest/download/install.sh | bash -s -- --gpu
EOF
  if config_has_realtime; then
    cat >&2 <<EOF

  Streaming recognition is at ${url/http/ws}/v1/realtime — see $url/docs.
  The Russian streaming models write no punctuation and no capitals.
EOF
  fi
}

uninstall_nanoasr() {
  escalate
  if systemd_running; then
    say "stopping and disabling $SERVICE"
    $SUDO systemctl disable --now "$SERVICE" 2>/dev/null || true
  fi
  # The GPU drop-in lives in a directory of its own beside the unit. It goes
  # whether or not the unit is still there, because a leftover one would
  # configure the next installation with libraries that are not.
  if [[ -d "$UNIT_DIR/$SERVICE.service.d" ]]; then
    $SUDO rm -rf "$UNIT_DIR/$SERVICE.service.d"
  fi
  if [[ -f "$UNIT" ]]; then
    $SUDO rm -f "$UNIT"
  fi
  if systemd_running; then
    $SUDO systemctl daemon-reload || true
  fi
  say "removing $PREFIX"
  $SUDO rm -rf "$PREFIX"
  if [[ "$PURGE" == "1" ]]; then
    say "removing $DATA_DIR and the account $SVC_USER"
    $SUDO rm -rf "$DATA_DIR"
    if id -u "$SVC_USER" >/dev/null 2>&1; then
      $SUDO userdel "$SVC_USER" 2>/dev/null || warn "the account $SVC_USER is still there"
    fi
  else
    say "kept $DATA_DIR — the models, the job database and the spool; --purge deletes it"
  fi
  say "done"
}

case "$ACTION" in
  install)   install_nanoasr ;;
  uninstall) uninstall_nanoasr ;;
esac

# The installation itself is done either way; this is about the GPU step, which
# either was asked for or was already in place. Failing out loud is the point:
# the service is running on the CPU and somebody is expecting otherwise.
if [[ "${GPU_FAILED:-0}" == "1" ]]; then
  warn "finished, but not on the GPU — see the message above"
  exit 1
fi
