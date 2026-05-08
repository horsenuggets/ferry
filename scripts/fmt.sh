#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

gofmt -w .

if ! command -v goimports >/dev/null 2>&1; then
  echo "goimports not found on PATH" >&2
  echo "install: go install golang.org/x/tools/cmd/goimports@latest" >&2
  exit 1
fi
goimports -w .
