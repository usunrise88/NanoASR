# syntax=docker/dockerfile:1

# FLAVOUR picks what goes under the same binary: the CPU-only build of the
# sherpa-onnx libraries, or the CUDA one plus a base image that carries the
# CUDA runtime.
#
#   docker build -f deploy/Dockerfile .                        # CPU
#   docker build -f deploy/Dockerfile --build-arg FLAVOUR=gpu .
#
# Declared here, before the first FROM, because that is the only scope a
# variable used in a FROM line can come from.
ARG FLAVOUR=cpu
# A CUDA runtime image, not a devel one: nothing is compiled in it, and the CUDA
# and cuDNN shared libraries are all the onnxruntime provider needs. It has to
# match the archive fetched below — CUDA 12 with cuDNN 9 — and the container
# needs --gpus all (or a device plugin) to see a device at all.
ARG CUDA_IMAGE=nvidia/cuda:12.8.1-cudnn-runtime-ubuntu24.04

# ---- SPA -------------------------------------------------------------------
FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---- server ----------------------------------------------------------------
# cgo is required: sherpa-onnx is a C++ library behind a C API. The version
# tracks the go directive in go.mod, because the image pins GOTOOLCHAIN=local:
# an older toolchain here does not download a newer one, it refuses to build.
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/web/dist ./internal/ui/dist
ARG VERSION=dev
# The shared libraries are copied out in the same step that builds, and not in
# one of its own, because the module cache is a cache mount rather than part of
# the image: outside a step that mounts it, /go/pkg/mod is an empty directory.
# Keeping both outputs in one layer also survives a cache hit that skips the
# build entirely.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -ldflags "-X main.version=${VERSION}" -o /out/nanoasr ./cmd/nanoasr && \
    mkdir -p /out/lib && \
    cp "$(go env GOMODCACHE)"/github.com/k2-fsa/sherpa-onnx-go-linux@*/lib/x86_64-unknown-linux-gnu/*.so /out/lib/

# ---- native libraries ------------------------------------------------------
# Two flavours of the same image, selected with --build-arg FLAVOUR=gpu.
#
# The difference between CPU and GPU recognition is not the binary: it is which
# build of the sherpa-onnx C API the dynamic loader finds. So the flavour is
# chosen here, by assembling the library directory, and once more below, by
# picking the runtime base image that carries CUDA. BuildKit builds only the
# stages the target needs, so a CPU build never downloads any of this.
FROM build AS libs-cpu

FROM libs-cpu AS libs-gpu
# SHERPA_VERSION has to match the C API the binary was linked against, which is
# the version in go.mod — the same check scripts/install-gpu.sh makes against an
# installed binary. The onnxruntime version is part of the archive's name
# upstream, and the checksum is measured from the published artefact. For CUDA
# 13 pass --build-arg CUDA_MAJOR=13 with its checksum from
# scripts/install-gpu.sh.
ARG SHERPA_VERSION=1.13.6
ARG ONNXRUNTIME_VERSION=1.27.1
ARG CUDA_MAJOR=12
ARG GPU_SHA256=0b13ee885cc2f6083ff21b4ca977cf2baf62af0caf33f597d9dc48938d6017c5
RUN apt-get update && apt-get install -y --no-install-recommends bzip2 && \
    rm -rf /var/lib/apt/lists/*
RUN name="sherpa-onnx-v${SHERPA_VERSION}-cuda-${CUDA_MAJOR}.x-cudnn-9.x-onnxruntime${ONNXRUNTIME_VERSION}-linux-x64-gpu" && \
    curl -fsSL -o /tmp/gpu.tar.bz2 \
      "https://github.com/k2-fsa/sherpa-onnx/releases/download/v${SHERPA_VERSION}/${name}.tar.bz2" && \
    echo "${GPU_SHA256}  /tmp/gpu.tar.bz2" | sha256sum -c - && \
    tar -xjf /tmp/gpu.tar.bz2 -C /out/lib --strip-components=2 --wildcards "*/lib/*.so" && \
    rm /tmp/gpu.tar.bz2 && \
    test -f /out/lib/libonnxruntime_providers_cuda.so

FROM libs-${FLAVOUR} AS libs

# ---- runtime ---------------------------------------------------------------
# The two bases, and the environment that goes with each. NANOASR_PROVIDER is
# set here rather than left to the configuration file because it describes the
# image: the GPU image has no CPU-only libraries in it to fall back to, and the
# CPU image would refuse to start if it were told to use CUDA.
FROM debian:bookworm-slim AS runtime-cpu
ENV NANOASR_PROVIDER=cpu \
    LD_LIBRARY_PATH=/usr/local/lib/nanoasr

FROM ${CUDA_IMAGE} AS runtime-gpu
ENV NANOASR_PROVIDER=cuda \
    LD_LIBRARY_PATH=/usr/local/lib/nanoasr:/usr/local/cuda/lib64

FROM runtime-${FLAVOUR}
# ffmpeg handles every input format the native WAV/PCM path does not. It is
# optional at runtime: without it the server still serves WAV and answers 415
# for the rest, rather than failing to start.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN useradd --system --uid 10001 --home /var/lib/nanoasr --create-home nanoasr

COPY --from=build /out/nanoasr /usr/local/bin/nanoasr
COPY --from=libs /out/lib/ /usr/local/lib/nanoasr/
COPY configs/nanoasr.example.yaml /etc/nanoasr/nanoasr.yaml

# LD_LIBRARY_PATH is set by the flavour stage above, because the GPU image needs
# the CUDA directory on it as well.
ENV NANOASR_CONFIG=/etc/nanoasr/nanoasr.yaml \
    NANOASR_MODELS_DIR=/var/lib/nanoasr/models \
    NANOASR_DB_PATH=/var/lib/nanoasr/nanoasr.db

USER nanoasr
WORKDIR /var/lib/nanoasr
VOLUME ["/var/lib/nanoasr"]
EXPOSE 8080

# `nanoasr version` links against the native libraries and calls into them, so a
# library set that cannot be loaded at all fails this. It does not check that
# CUDA works — that is `nanoasr gpu`, which the server also does for itself at
# startup and refuses to run on a mismatch.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
    CMD ["/usr/local/bin/nanoasr", "version"]

ENTRYPOINT ["/usr/local/bin/nanoasr"]
CMD ["serve"]
