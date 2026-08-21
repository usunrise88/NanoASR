# syntax=docker/dockerfile:1

# ---- SPA -------------------------------------------------------------------
FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# ---- server ----------------------------------------------------------------
# cgo is required: sherpa-onnx is a C++ library behind a C API.
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/web/dist ./internal/ui/dist
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -ldflags "-X main.version=${VERSION}" -o /out/nanoasr ./cmd/nanoasr

# The shared libraries live in the module cache; copy them next to the binary
# so the runtime image does not need Go at all.
RUN mkdir -p /out/lib && \
    cp "$(go env GOMODCACHE)"/github.com/k2-fsa/sherpa-onnx-go-linux@*/lib/x86_64-unknown-linux-gnu/*.so /out/lib/

# ---- runtime ---------------------------------------------------------------
FROM debian:bookworm-slim
# ffmpeg handles every input format the native WAV/PCM path does not. It is
# optional at runtime: without it the server still serves WAV and answers 415
# for the rest, rather than failing to start.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN useradd --system --uid 10001 --home /var/lib/nanoasr --create-home nanoasr

COPY --from=build /out/nanoasr /usr/local/bin/nanoasr
COPY --from=build /out/lib/ /usr/local/lib/nanoasr/
COPY configs/nanoasr.example.yaml /etc/nanoasr/nanoasr.yaml

ENV LD_LIBRARY_PATH=/usr/local/lib/nanoasr \
    NANOASR_CONFIG=/etc/nanoasr/nanoasr.yaml \
    NANOASR_MODELS_DIR=/var/lib/nanoasr/models \
    NANOASR_DB_PATH=/var/lib/nanoasr/nanoasr.db

USER nanoasr
WORKDIR /var/lib/nanoasr
VOLUME ["/var/lib/nanoasr"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
    CMD ["/usr/local/bin/nanoasr", "version"]

ENTRYPOINT ["/usr/local/bin/nanoasr"]
CMD ["serve"]
