#!/usr/bin/env bash
# Build a release artefact. Prefer `make dist`; this script exists so CI and
# release tooling can call the same steps directly, and so the RUNPATH
# incantation has exactly one definition — quoting $ORIGIN correctly through
# make, sh and the linker is easy to get subtly wrong, and a wrong one only
# shows up on the target machine.
#
#   GOOS=linux|windows  ARCH=amd64  UI=1|0  VERSION=...  OUT=dist
#
# One binary, two shapes. Linux gets a tarball whose RUNPATH points beside the
# binary; Windows gets a zip, because the loader there already looks next to the
# .exe and needs no help. Both carry the native libraries: sherpa-onnx ships
# only as shared objects, so "copy one binary" was never going to work.
set -euo pipefail

GOOS="${GOOS:-linux}"
ARCH="${ARCH:-amd64}"
UI="${UI:-1}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="${OUT:-dist}"

case "$GOOS" in
  linux)   LIBDIR="$(go env GOMODCACHE)/github.com/k2-fsa/sherpa-onnx-go-linux@v1.13.6/lib/x86_64-unknown-linux-gnu" ;;
  windows) LIBDIR="$(go env GOMODCACHE)/github.com/k2-fsa/sherpa-onnx-go-windows@v1.13.6/lib/x86_64-pc-windows-gnu" ;;
  *) echo "unsupported GOOS: $GOOS" >&2; exit 1 ;;
esac

# The libraries live in the module cache and this script looks for them before
# it builds anything, so on a cold checkout they have to be fetched first. `go
# build` would have done it, but only after the check below had already failed —
# which is exactly how a fresh CI runner failed on its first release.
GOOS="$GOOS" go mod download

if [[ ! -d "$LIBDIR" ]]; then
  echo "native libraries not found at $LIBDIR after 'go mod download'" >&2
  exit 1
fi

# The UI is embedded at compile time from internal/ui/dist, which holds a
# committed placeholder page in a fresh checkout. A build that means to ship the
# SPA has to have staged it there first (make web); one that does not is built
# with -tags noui so the placeholder is not mistaken for a broken UI.
tags=()
suffix=""
if [[ "$UI" != "1" ]]; then
  tags=(-tags noui)
  suffix="-noui"
elif [[ ! -d internal/ui/dist/assets ]]; then
  echo "UI=1 but internal/ui/dist/assets is missing — run 'make web' first" >&2
  exit 1
fi

STAGE="$OUT/pkg-$GOOS$suffix"
rm -rf "$STAGE"
mkdir -p "$STAGE"

if [[ "$GOOS" == "linux" ]]; then
  # $ORIGIN/lib is prepended to RUNPATH so the binary finds the libraries beside
  # itself rather than in the module cache of the build machine.
  mkdir -p "$STAGE/lib"
  CGO_LDFLAGS='-Wl,-rpath,$ORIGIN/lib' \
    GOOS=linux GOARCH="$ARCH" CGO_ENABLED=1 \
    go build "${tags[@]}" -ldflags "-X main.version=$VERSION" -o "$STAGE/nanoasr" ./cmd/nanoasr
  cp "$LIBDIR"/*.so "$STAGE/lib/"
  chmod +w "$STAGE"/lib/*.so
else
  GOOS=windows GOARCH="$ARCH" CGO_ENABLED=1 \
    go build "${tags[@]}" -ldflags "-X main.version=$VERSION" -o "$STAGE/nanoasr.exe" ./cmd/nanoasr
  cp "$LIBDIR"/*.dll "$STAGE/"
  chmod +w "$STAGE"/*.dll
fi

cp configs/nanoasr.example.yaml "$STAGE/nanoasr.yaml"
cp README.md "$STAGE/README.md"
if [[ "$GOOS" == "linux" ]]; then
  cp deploy/nanoasr.service "$STAGE/"
fi

NAME="nanoasr-${VERSION}-${GOOS}-${ARCH}${suffix}"
if [[ "$GOOS" == "linux" ]]; then
  ARCHIVE="${NAME}.tar.gz"
  tar -C "$STAGE" -czf "$ARCHIVE" .
else
  ARCHIVE="${NAME}.zip"
  rm -f "$ARCHIVE"
  # Whichever archiver this machine has. A Windows runner has 7z and
  # PowerShell but not necessarily zip, and a Linux box cross-compiling for
  # Windows has zip but neither of the others.
  abs="$(pwd)/$ARCHIVE"
  if command -v zip >/dev/null; then
    (cd "$STAGE" && zip -qr "$abs" .)
  elif command -v 7z >/dev/null; then
    (cd "$STAGE" && 7z a -tzip -bso0 -bsp0 "$abs" ./*)
  elif command -v powershell >/dev/null; then
    powershell -NoProfile -Command \
      "Compress-Archive -Path '$STAGE/*' -DestinationPath '$abs' -Force"
  else
    echo "no archiver found: install zip, 7z or PowerShell" >&2
    exit 1
  fi
fi

rm -rf "$STAGE"
echo "$ARCHIVE"
