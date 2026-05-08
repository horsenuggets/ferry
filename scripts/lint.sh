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

if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "golangci-lint not found on PATH" >&2
  echo "install: https://golangci-lint.run/welcome/install/" >&2
  exit 1
fi
golangci-lint run

# Shell linting: check our bash scripts with shellcheck if available. CI runs
# shellcheck unconditionally; locally it's best-effort.
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck scripts/install.sh scripts/uninstall.sh tests/scripts/install_test.sh
else
  echo "shellcheck not found on PATH; skipping shell-script lint"
fi
