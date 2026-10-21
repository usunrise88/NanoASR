#!/usr/bin/env bash
#
# Install what building NanoASR with its web UI takes on a Mac, through Homebrew.
#
#   Go       the version go.mod asks for
#   Node     20.19+ or 22.13+ (Vite 8 and ESLint 10 set the floor)
#   ffmpeg   not for the build: the server needs it to read anything but WAV
#
# The C/C++ compiler that cgo links sherpa-onnx with comes from the Xcode
# Command Line Tools, and Homebrew is the installer here. Both are checked and
# reported, never installed behind your back.
#
#   scripts/setup-macos-build.sh [--check] [--no-ffmpeg] [--no-fetch]
set -euo pipefail

CHECK=0
FFMPEG=1
FETCH=1
FAILED=0

usage() {
  cat <<'EOF'
usage: scripts/setup-macos-build.sh [--check] [--no-ffmpeg] [--no-fetch]

  --check      report what is missing and install nothing
  --no-ffmpeg  do not install ffmpeg
  --no-fetch   do not download Go modules and npm packages afterwards
EOF
}

for arg in "$@"; do
  case "$arg" in
    --check)     CHECK=1 ;;
    --no-ffmpeg) FFMPEG=0 ;;
    --no-fetch)  FETCH=0 ;;
    -h|--help)   usage; exit 0 ;;
    *)           usage >&2; exit 2 ;;
  esac
done

[[ "$(uname -s)" == "Darwin" ]] || { echo "this script is for macOS" >&2; exit 1; }
[[ "$EUID" -ne 0 ]] || { echo "do not run it as root: Homebrew refuses to" >&2; exit 1; }

cd "$(dirname "${BASH_SOURCE[0]}")/.."

step() { printf '\n==> %s\n' "$*"; }
ok()   { printf '    ok: %s\n' "$*"; }
need() { printf '    missing: %s\n' "$*"; if (( CHECK )); then FAILED=1; fi; }
run()  {
  if (( CHECK )); then printf '    would run: %s\n' "$*"; return 0; fi
  printf '    $ %s\n' "$*"
  "$@"
}

# Major and minor of go1.25.3 or 22.13.1; anything after the digits is dropped.
major() { local v=${1#go}; echo "${v%%.*}"; }
minor() { local v=${1#go}; v=${v#*.}; v=${v%%.*}; echo "${v%%[!0-9]*}"; }

GO_MIN="$(awk '$1 == "go" { print $2; exit }' go.mod)"

# Asked from / with the local toolchain: inside this module an older go would
# otherwise start downloading the toolchain go.mod names, and the answer would
# be that one's version rather than the one installed.
go_version() { (cd / && GOTOOLCHAIN=local go version 2>/dev/null) | awk '{ print $3 }'; }

go_ok() {
  command -v go >/dev/null || return 1
  local have maj min wmaj wmin
  have="$(go_version)"
  [[ -n "$have" ]] || return 1
  maj="$(major "$have")"; min="$(minor "$have")"
  wmaj="$(major "$GO_MIN")"; wmin="$(minor "$GO_MIN")"
  (( maj > wmaj || (maj == wmaj && min >= wmin) ))
}

node_ok() {
  command -v node >/dev/null || return 1
  local have maj min
  have="$(node -p 'process.versions.node')" || return 1
  maj="$(major "$have")"; min="$(minor "$have")"
  (( maj >= 24 || (maj == 22 && min >= 13) || (maj == 20 && min >= 19) ))
}

step "C/C++ compiler (Xcode Command Line Tools)"
if xcode-select -p >/dev/null 2>&1; then
  ok "$(clang --version | head -n1)"
else
  need "Xcode Command Line Tools"
  if (( ! CHECK )); then
    xcode-select --install || true
    echo "    finish the installer window that just opened, then run this script again."
    exit 1
  fi
fi

step "Homebrew"
if ! command -v brew >/dev/null; then
  for b in /opt/homebrew/bin/brew /usr/local/bin/brew; do
    if [[ -x "$b" ]]; then eval "$("$b" shellenv)"; break; fi
  done
fi
if command -v brew >/dev/null; then
  ok "$(brew --version | head -n1)"
else
  need "Homebrew"
  echo "    install it from https://brew.sh, then run this script again."
  if (( ! CHECK )); then exit 1; fi
fi

step "Go ($GO_MIN or newer, as go.mod asks)"
if go_ok; then
  ok "$(cd / && GOTOOLCHAIN=local go version)"
else
  need "Go"
  if brew list --formula go >/dev/null 2>&1; then run brew upgrade go; else run brew install go; fi
  if (( ! CHECK )); then
    hash -r
    go_ok || { echo "    the go on PATH is still too old: $(command -v go || echo none)" >&2; FAILED=1; }
  fi
fi

step "Node (20.19+ or 22.13+)"
if node_ok; then
  ok "node $(node -p 'process.versions.node'), npm $(npm --version)"
else
  need "Node"
  if brew list --formula node >/dev/null 2>&1; then
    run brew upgrade node
  else
    # node@22 is keg-only: without the link it is installed and not on PATH.
    run brew install node@22
    run brew link --overwrite --force node@22
  fi
  if (( ! CHECK )); then
    hash -r
    node_ok || { echo "    the node on PATH is still too old: $(command -v node || echo none)" >&2; FAILED=1; }
  fi
fi

if (( FFMPEG )); then
  step "ffmpeg (runtime only: without it the server reads WAV and raw PCM)"
  if command -v ffmpeg >/dev/null; then
    ok "$(ffmpeg -version | head -n1)"
  else
    need "ffmpeg"
    run brew install ffmpeg
  fi
fi

if (( FETCH && ! CHECK && ! FAILED )); then
  step "Go modules and npm packages, so the first build does not wait on the network"
  run go mod download
  run npm --prefix web ci --no-audit --no-fund
fi

if (( FAILED )); then
  step "Not ready"
  if (( CHECK )); then echo "    run without --check to install what is missing."; fi
  exit 1
fi

arch="$(uname -m)"
[[ "$arch" == "arm64" ]] || arch="amd64"
step "Ready"
echo "    Archive with the web UI, in the repository root:"
echo "        GOOS=darwin ARCH=$arch make dist"
