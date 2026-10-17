# bootwatcherd: boot.watch's monitor and status page.

BIN     := dist/bootwatcherd
# the release tag (v0.1.0), or where the build is past it (v0.1.0-3-gabc1234-dirty)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test check clean

# One static binary, standard library only: no libc to match on the server.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) .

test:
	go test ./...

check:
	go vet ./...
	go test ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }
	@sh tools/scrub-check.sh

clean:
	rm -rf dist
