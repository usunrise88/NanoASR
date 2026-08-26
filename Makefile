# NanoASR build.
#
# The server is one binary with the SPA embedded. Two deployment artefacts:
# a Docker image (primary) and a relocatable tar.gz for systemd.

SHELL       := /bin/bash
GO          ?= go
NPM         ?= npm
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)
DIST        := dist
UI_DIST     := internal/ui/dist

.PHONY: all web build build-noui dist dist-noui docker lint test test-race test-integration \
	load models testdata golden-update run clean tidy help

all: web build

## web: build the SPA and stage it for embedding
web:
	cd web && $(NPM) ci --no-audit --no-fund
	cd web && $(NPM) run build
	rm -rf $(UI_DIST)
	cp -r web/dist $(UI_DIST)

## build: build the server binary (run `make web` first, or it starts headless)
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/nanoasr ./cmd/nanoasr

## build-noui: build without the SPA
build-noui:
	$(GO) build -tags noui -ldflags "$(LDFLAGS)" -o $(DIST)/nanoasr ./cmd/nanoasr

## dist: relocatable archive with the native libraries beside the binary
#
# Delegates to scripts/dist.sh so the RUNPATH incantation has exactly one
# definition: quoting $ORIGIN correctly through make, sh and the linker is easy
# to get subtly wrong, and a wrong one only shows up on the target machine.
#
# GOOS=windows and UI=0 select the other three release shapes; the release
# workflow calls the same script with the same variables.
dist: web
	VERSION=$(VERSION) ./scripts/dist.sh

## dist-noui: the same archive without the SPA (no web build needed)
dist-noui:
	VERSION=$(VERSION) UI=0 ./scripts/dist.sh

## docker: build the container image
docker:
	docker build -f deploy/Dockerfile -t nanoasr:$(VERSION) .

## lint: vet and lint both halves of the codebase
lint:
	$(GO) vet ./...
	@gofmt -l $$(git ls-files '*.go') | tee /dev/stderr | (! read)
	cd web && $(NPM) run lint
	cd web && $(NPM) run typecheck

## models: download the models the dev loop and integration tests need
models:
	./scripts/fetch-dev-models.sh

## testdata: download and derive the integration test audio
testdata:
	./scripts/fetch-testdata.sh

## test: unit tests
test:
	$(GO) test ./...

## test-integration: end-to-end tests against real weights (needs models testdata)
test-integration:
	$(GO) test -tags integration ./... -v -run "Integration|M1Report"

## golden-update: regenerate the golden transcript from the current model
golden-update:
	$(GO) test -tags integration ./internal/pipeline/ \
		-run TestIntegrationTranscribesReferenceClip -update-golden -v

## load: 100 concurrent 5-minute files against a running server (SPEC §15)
#
# Not part of `make test`, and not in CI: it takes thirty minutes by design,
# because a leak is only visible over time. Start a server first.
load:
	./scripts/loadtest.sh

## test-race: the pool, the queue and the governor are only correct under -race
test-race:
	$(GO) test -race ./...

## run: run the server against the development config
run: build
	$(DIST)/nanoasr serve -config configs/nanoasr.dev.yaml

tidy:
	$(GO) mod tidy

clean:
	rm -rf $(DIST) web/dist $(UI_DIST)/assets nanoasr-*.tar.gz

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
