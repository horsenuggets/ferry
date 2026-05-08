#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="$(cat VERSION)"
LDFLAGS="-X github.com/horsenuggets/ferry/src/version.Version=$VERSION"

mkdir -p dist
go build -ldflags="$LDFLAGS" -o dist/ferry ./src/cmd/ferry
