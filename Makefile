.PHONY: build test race lint bench fmt tidy run dist checksums clean-dist

# Release build configuration. VERSION defaults to the current git description and
# can be overridden (the release workflow passes the tag, e.g. VERSION=v0.1.0).
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DATE      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS   := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)
DIST      := dist
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
# sha256sum on Linux, shasum -a 256 on macOS.
SHASUM    := $(shell command -v sha256sum >/dev/null 2>&1 && echo "sha256sum" || echo "shasum -a 256")

build:
	go build ./...

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run

bench:
	go test -bench . ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

run:
	go run ./cmd/intreccio

# dist cross-compiles the CLI for every target in PLATFORMS into $(DIST)/.
# Pure Go: CGO_ENABLED=0 keeps the no-cgo invariant and makes cross-compilation
# work with only the Go toolchain (no C cross-toolchains, no extra tools).
dist: clean-dist
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=; if [ "$$os" = "windows" ]; then ext=.exe; fi; \
		out=$(DIST)/intreccio_$(VERSION)_$${os}_$${arch}$$ext; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/intreccio || exit 1; \
	done

# checksums writes a SHA-256 manifest of the dist artifacts.
checksums:
	@cd $(DIST) && $(SHASUM) intreccio_* > checksums.txt && echo "wrote $(DIST)/checksums.txt"

clean-dist:
	rm -rf $(DIST)
