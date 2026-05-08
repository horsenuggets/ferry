#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

out="$(gofmt -l .)"
if [ -n "$out" ]; then
  echo "gofmt found issues:"
  echo "$out"
  exit 1
fi

go vet ./...

golangci-lint run
