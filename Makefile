# Build entry points. On Windows without make, run the
# equivalent go commands directly: `go test ./...`, `go vet ./...`, `go build -o bin/ ./cmd/...`.

GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X github.com/DragonDrew/ddum/internal/buildinfo.Version=$(VERSION)
BUILDENV := CGO_ENABLED=0

.PHONY: all build build-linux test test-race test-pkg lint vuln check fmt e2e docker clean

all: lint test build

## build: binaries for this machine, into bin/
build:
	$(BUILDENV) $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/ ./cmd/...

## build-linux: linux/amd64 and linux/arm64 binaries, into bin/linux-<arch>/
build-linux:
	$(BUILDENV) GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/linux-amd64/ ./cmd/...
	$(BUILDENV) GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/linux-arm64/ ./cmd/...

## test: every test that needs no Docker
test:
	$(GO) test ./...

## test-race: the same with the race detector; needs a C toolchain
test-race:
	CGO_ENABLED=1 $(GO) test -race ./...

## vuln: known vulnerabilities in the code paths ddum uses
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

## test-pkg: tests for one package tree while iterating, e.g. make test-pkg PKG=./internal/bot/...
test-pkg:
	@test -n "$(PKG)" || { echo "usage: make test-pkg PKG=./internal/bot/..."; exit 1; }
	$(GO) test $(PKG)

## lint: vet plus a gofmt check
lint:
	$(GO) vet ./...
	$(GO) vet -tags rig ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

## check: what CI runs: lint, the Docker-free tests and the Linux builds (e2e is separate)
check: lint test build-linux

fmt:
	gofmt -w .

## e2e: end-to-end rig against real Docker; needs a linux/amd64 Docker host
e2e:
	bash deploy/test-rig/run.sh

## docker: container images ddum-bot:$(VERSION) and ddum-agent:$(VERSION) for this machine's platform.
## TAGS=rig adds the fake Discord to the bot, for the end-to-end rig.
docker:
	docker build -f deploy/Dockerfile.bot --build-arg VERSION=$(VERSION) --build-arg BUILD_TAGS=$(TAGS) -t ddum-bot:$(VERSION) .
	docker build -f deploy/Dockerfile.agent --build-arg VERSION=$(VERSION) -t ddum-agent:$(VERSION) .

clean:
	rm -rf bin/
