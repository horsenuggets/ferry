#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="$(cat VERSION)"
LDFLAGS="-X github.com/horsenuggets/ferry/src/version.Version=$VERSION"
PKG="./src/cmd/ferry"

mkdir -p dist

GOOS=darwin  GOARCH=arm64 go build -ldflags="$LDFLAGS" -o dist/ferry-darwin-arm64       "$PKG"
GOOS=darwin  GOARCH=amd64 go build -ldflags="$LDFLAGS" -o dist/ferry-darwin-amd64       "$PKG"
GOOS=linux   GOARCH=amd64 go build -ldflags="$LDFLAGS" -o dist/ferry-linux-amd64        "$PKG"
GOOS=linux   GOARCH=arm64 go build -ldflags="$LDFLAGS" -o dist/ferry-linux-arm64        "$PKG"
GOOS=windows GOARCH=amd64 go build -ldflags="$LDFLAGS" -o dist/ferry-windows-amd64.exe  "$PKG"
