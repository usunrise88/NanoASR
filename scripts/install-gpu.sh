#!/usr/bin/env bash
#
# Put NanoASR's recognition on an NVIDIA GPU.
#
#   curl -fsSL https://github.com/usunrise88/NanoASR/releases/latest/download/install-gpu.sh | sudo bash
#
# What this installs is not a different NanoASR: it is the same binary with
# different native libraries under it. sherpa-onnx ships two builds of its C
# API — one against a CPU-only onnxruntime, one against a CUDA one — and which
# of them a process uses is decided by the dynamic loader, not by a setting.
# So this script fetches the CUDA build, puts it in a directory of its own, and
# points the service at it with a systemd drop-in. The CPU libraries stay where
# they are, which is what makes --revert a deletion rather than a reinstall.
#
# It refuses rather than guesses in two places: when the archive's sherpa-onnx
# version does not match the installed binary's (the C ABI has to agree), and
# when the libraries cannot be loaded on this machine (no driver, no cuDNN) —
# because the alternative is a service that starts, says nothing, and quietly
# runs on the CPU.
#
# Requires CUDA 12 (or 13, with --cuda 13) and cuDNN 9 on the library path, and
# an NVIDIA driver new enough for them.
set -euo pipefail

PREFIX="${NANOASR_PREFIX:-/opt/nanoasr}"
SERVICE="${NANOASR_SERVICE:-nanoasr}"
CUDA="${NANOASR_CUDA:-12}"
SHERPA="${NANOASR_SHERPA_VERSION:-1.13.6}"
ONNXRUNTIME="${NANOASR_ONNXRUNTIME_VERSION:-1.27.1}"
SHA256="${NANOASR_GPU_SHA256:-}"
RESTART="${NANOASR_START:-1}"
VERIFY=1
ACTION=install
if [[ "${NANOASR_GPU_REVERT:-}" == "1" ]]; then ACTION=revert; fi

UNIT_DIR=/etc/systemd/system
SUDO=""
TMP=""

say()  { printf '==> %s\n' "$*" >&2; }
warn() { printf '    warning: %s\n' "$*" >&2; }
die()  { printf 'install-gpu.sh: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
systemd_running() { have systemctl && [[ -d /run/systemd/system ]]; }

usage() {
  cat >&2 <<EOF
Install the CUDA build of the sherpa-onnx libraries and point NanoASR at them.

  --prefix DIR      where NanoASR is installed            ($PREFIX)
  --service NAME    systemd unit to reconfigure            ($SERVICE)
  --cuda 12|13      CUDA major version to match            ($CUDA)
  --sherpa VERSION  sherpa-onnx version to fetch           ($SHERPA)
  --sha256 HEX      checksum, for a version not pinned here
  --no-verify       install without checking the checksum (not advised)
  --no-restart      install and configure, restart nothing
  --revert          remove the GPU libraries and the drop-in
  -h, --help        this

Environment: NANOASR_PREFIX, NANOASR_SERVICE, NANOASR_CUDA,
NANOASR_SHERPA_VERSION, NANOASR_ONNXRUNTIME_VERSION, NANOASR_GPU_SHA256,
NANOASR_START=0, NANOASR_GPU_REVERT=1. A flag wins over one.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --prefix)     PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
    --service)    SERVICE="${2:?--service needs a name}"; shift 2 ;;
    --cuda)       CUDA="${2:?--cuda needs 12 or 13}"; shift 2 ;;
    --sherpa)     SHERPA="${2:?--sherpa needs a version}"; shift 2 ;;
    --sha256)     SHA256="${2:?--sha256 needs a checksum}"; shift 2 ;;
    --no-verify)  VERIFY=0; shift ;;
    --no-restart) RESTART=0; shift ;;
    --revert)     ACTION=revert; shift ;;
    -h|--help)    usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

GPU_DIR="$PREFIX/lib-gpu"
DROPIN_DIR="$UNIT_DIR/$SERVICE.service.d"
DROPIN="$DROPIN_DIR/10-gpu.conf"
BINARY="$PREFIX/nanoasr"

cleanup() {
  [[ -n "$TMP" ]] && rm -rf "$TMP"
  return 0
}
trap cleanup EXIT

escalate() {
  if [[ "$(id -u)" -eq 0 ]]; then
    SUDO=""
    return
  fi
  have sudo || die "this needs root and sudo is not installed; run it as root"
  SUDO="sudo"
  say "using sudo for the privileged steps"
}

# Checksums measured from the published archives. A version that is not in this
# table can still be installed, with --sha256 or --no-verify, and says so.
archive_sha256() {
  case "$SHERPA-$ONNXRUNTIME-cuda$CUDA" in
    1.13.6-1.27.1-cuda12)
      echo 0b13ee885cc2f6083ff21b4ca977cf2baf62af0caf33f597d9dc48938d6017c5 ;;
    1.13.6-1.27.1-cuda13)
      echo a1b18fab3000752790212c0081bf8fcce65c211414e232cd30955e1699ff3de1 ;;
    *) echo "" ;;
  esac
}

# --- preconditions ----------------------------------------------------------

preflight() {
  [[ "$(uname -s)" == "Linux" ]] || die "the GPU build is Linux only"
  [[ "$(uname -m)" == "x86_64" ]] || die "the GPU build is x86-64 only, this is $(uname -m)"
  case "$CUDA" in
    12|13) ;;
    *) die "--cuda takes 12 or 13, got $CUDA" ;;
  esac
  for tool in curl tar sha256sum; do
    have "$tool" || die "$tool is required"
  done
  [[ -x "$BINARY" ]] || die "no NanoASR at $BINARY; install it first (scripts/install.sh)"

  # The libraries replace the ones the binary was linked against, so the C API
  # has to be the same version. A mismatch here is a crash at the first model
  # load, with a message about a missing symbol that explains nothing.
  local installed
  installed="$("$BINARY" version 2>/dev/null | awk '/^sherpa-onnx/ {print $2}')"
  if [[ -z "$installed" ]]; then
    warn "could not read the sherpa-onnx version from $BINARY"
  elif [[ "$installed" != "$SHERPA" ]]; then
    die "$BINARY was built against sherpa-onnx $installed but this would install $SHERPA;
    pass --sherpa $installed, or update NanoASR first"
  fi

  if ! compgen -G "/dev/nvidia[0-9]*" >/dev/null && ! have nvidia-smi; then
    warn "no NVIDIA device and no nvidia-smi on this machine: the libraries will install,"
    warn "and the verification step below will tell you whether they can be used"
  fi
}

# --- install ----------------------------------------------------------------

fetch() {
  local url="$1" out="$2"
  say "downloading $(basename "$url")"
  curl -fSL -# -o "$out" "$url"
}

verify() {
  local file="$1" want="$2"
  if [[ "$VERIFY" -eq 0 ]]; then
    warn "checksum not verified (--no-verify)"
    return
  fi
  if [[ -z "$want" ]]; then
    die "no checksum is pinned for sherpa-onnx $SHERPA with onnxruntime $ONNXRUNTIME on CUDA $CUDA;
    pass --sha256 <hex>, or --no-verify to install it unchecked"
  fi
  local got
  got="$(sha256sum "$file" | cut -d' ' -f1)"
  [[ "$got" == "$want" ]] || die "checksum mismatch: expected $want, got $got"
  say "checksum ok"
}

install_libraries() {
  local name="sherpa-onnx-v$SHERPA-cuda-$CUDA.x-cudnn-9.x-onnxruntime$ONNXRUNTIME-linux-x64-gpu"
  local url="https://github.com/k2-fsa/sherpa-onnx/releases/download/v$SHERPA/$name.tar.bz2"

  TMP="$(mktemp -d)"
  fetch "$url" "$TMP/gpu.tar.bz2"
  verify "$TMP/gpu.tar.bz2" "$(archive_sha256)"

  # Only the shared objects: the archive also carries headers and command-line
  # tools, and 400 MB of them is not something to leave on a server.
  say "unpacking the libraries"
  mkdir -p "$TMP/stage"
  tar -xjf "$TMP/gpu.tar.bz2" -C "$TMP/stage" --strip-components=2 --wildcards "*/lib/*.so"
  [[ -f "$TMP/stage/libonnxruntime_providers_cuda.so" ]] ||
    die "the archive has no CUDA provider library; is $name a GPU build?"

  # Staged beside the destination and moved into place, so an interrupted
  # install never leaves a half-populated library directory for the loader to
  # find.
  $SUDO rm -rf "$GPU_DIR.new" "$GPU_DIR.old"
  $SUDO mkdir -p "$GPU_DIR.new"
  $SUDO cp "$TMP"/stage/*.so "$GPU_DIR.new/"
  $SUDO chmod 0644 "$GPU_DIR.new"/*.so
  if [[ -d "$GPU_DIR" ]]; then
    $SUDO mv "$GPU_DIR" "$GPU_DIR.old"
  fi
  $SUDO mv "$GPU_DIR.new" "$GPU_DIR"
  $SUDO rm -rf "$GPU_DIR.old"
  say "installed $(ls -1 "$GPU_DIR" | wc -l) libraries in $GPU_DIR"
}

# check_runtime asks the binary itself whether the provider can be served, with
# the new libraries ahead of the old ones. Running this before the drop-in is
# written is the whole point: a verification that happens after the service has
# been reconfigured is a verification of a broken service.
check_runtime() {
  say "checking whether CUDA can be served"
  if ! LD_LIBRARY_PATH="$GPU_DIR" "$BINARY" gpu -provider cuda; then
    die "the GPU libraries are installed at $GPU_DIR but cannot be used on this machine.
    Nothing has been reconfigured; the service is still running on the CPU.
    Install the CUDA $CUDA and cuDNN 9 runtime libraries and run this again,
    or remove them with --revert."
  fi
}

configure_service() {
  if ! systemd_running; then
    warn "systemd is not running here, so nothing was reconfigured"
    say  "run the server with LD_LIBRARY_PATH=$GPU_DIR and NANOASR_PROVIDER=cuda"
    return
  fi
  if ! $SUDO systemctl cat "$SERVICE" >/dev/null 2>&1; then
    warn "there is no $SERVICE service, so nothing was reconfigured"
    say  "run the server with LD_LIBRARY_PATH=$GPU_DIR and NANOASR_PROVIDER=cuda"
    return
  fi

  say "pointing $SERVICE at the GPU libraries"
  $SUDO mkdir -p "$DROPIN_DIR"
  $SUDO tee "$DROPIN" >/dev/null <<EOF
# Written by install-gpu.sh. Delete this file (or run install-gpu.sh --revert)
# to put NanoASR back on the CPU.
#
# LD_LIBRARY_PATH comes before the RUNPATH the binary carries, which is how the
# CUDA build of sherpa-onnx is used without reinstalling anything. The provider
# is named explicitly because the server refuses to start on a mismatch rather
# than falling back to the CPU in silence.
[Service]
Environment=LD_LIBRARY_PATH=$GPU_DIR
Environment=NANOASR_PROVIDER=cuda
EOF
  $SUDO systemctl daemon-reload

  if [[ "$RESTART" -eq 1 ]]; then
    say "restarting $SERVICE"
    $SUDO systemctl restart "$SERVICE"
  else
    say "not restarted (--no-restart); the change applies on the next start"
  fi
}

revert() {
  if systemd_running && $SUDO systemctl cat "$SERVICE" >/dev/null 2>&1; then
    if [[ -f "$DROPIN" ]]; then
      say "removing $DROPIN"
      $SUDO rm -f "$DROPIN"
      $SUDO rmdir --ignore-fail-on-non-empty "$DROPIN_DIR" 2>/dev/null || true
      $SUDO systemctl daemon-reload
      if [[ "$RESTART" -eq 1 ]]; then
        say "restarting $SERVICE on the CPU"
        $SUDO systemctl restart "$SERVICE"
      fi
    fi
  fi
  if [[ -d "$GPU_DIR" ]]; then
    say "removing $GPU_DIR"
    $SUDO rm -rf "$GPU_DIR"
  fi
  say "NanoASR is back on the CPU libraries it shipped with"
}

summary() {
  cat >&2 <<EOF

    NanoASR is configured to recognise on the GPU.

      libraries   $GPU_DIR
      drop-in     $DROPIN
      check       LD_LIBRARY_PATH=$GPU_DIR $BINARY gpu
      service     systemctl status $SERVICE
      revert      install-gpu.sh --revert

    A GPU is worth having here for throughput rather than for the latency of a
    single stream: the realtime engine decodes every open session in one batched
    call, and that is what fills a device. Raise realtime.max_sessions and
    asr.batch.max_size once this is running.
EOF
}

main() {
  escalate
  if [[ "$ACTION" == "revert" ]]; then
    revert
    return
  fi
  preflight
  install_libraries
  check_runtime
  configure_service
  summary
}

main "$@"
