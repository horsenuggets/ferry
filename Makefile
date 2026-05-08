VERSION := $(shell cat VERSION)
LDFLAGS := -X github.com/horsenuggets/ferry/internal/version.Version=$(VERSION)
PKG     := ./cmd/ferry

.PHONY: all build test lint fmt clean cross

all: build

build:
	go build -ldflags="$(LDFLAGS)" -o dist/ferry $(PKG)

test:
	go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .
	go vet ./...

clean:
	rm -rf dist/

cross:
	mkdir -p dist
	GOOS=darwin  GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/ferry-darwin-arm64  $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/ferry-darwin-amd64  $(PKG)
	GOOS=linux   GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/ferry-linux-amd64   $(PKG)
	GOOS=linux   GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o dist/ferry-linux-arm64   $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o dist/ferry-windows-amd64.exe $(PKG)
