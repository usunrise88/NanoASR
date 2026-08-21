#!/usr/bin/env bash
# Build the relocatable tarball. Prefer `make dist`; this script exists so CI
# and release tooling can call the same steps directly.
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
ARCH="${ARCH:-amd64}"
OUT="${OUT:-dist}"
LIBDIR="$(go env GOMODCACHE)/github.com/k2-fsa/sherpa-onnx-go-linux@v1.13.6/lib/x86_64-unknown-linux-gnu"

if [[ ! -d "$LIBDIR" ]]; then
  echo "native libraries not found at $LIBDIR — run 'go mod download' first" >&2
  exit 1
fi

mkdir -p "$OUT/lib"

# $ORIGIN/lib is prepended to RUNPATH so the binary finds the libraries beside
# itself rather than in the module cache of the build machine.
CGO_LDFLAGS='-Wl,-rpath,$ORIGIN/lib' \
  go build -ldflags "-X main.version=$VERSION" -o "$OUT/nanoasr" ./cmd/nanoasr

cp "$LIBDIR"/*.so "$OUT/lib/"
chmod +w "$OUT"/lib/*.so
cp configs/nanoasr.example.yaml "$OUT/nanoasr.yaml"
cp deploy/nanoasr.service "$OUT/"

tar -C "$OUT" -czf "nanoasr-${VERSION}-linux-${ARCH}.tar.gz" \
  nanoasr lib nanoasr.yaml nanoasr.service

echo "nanoasr-${VERSION}-linux-${ARCH}.tar.gz"
