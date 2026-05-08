#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

gofmt -w .

if command -v goimports >/dev/null 2>&1; then
  goimports -w .
else
  echo "goimports not installed; skipping (gofmt only)"
fi
