# Convenience targets. Plain "go build ./..." still works; these add the
# version stamp that "ark --version" reports.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build build-server test clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/ark ./cmd/ark

build-server:
	go build -ldflags "$(LDFLAGS)" -o bin/ark-server ./cmd/ark-server

test:
	go test ./...

clean:
	rm -rf bin dist
