#!/usr/bin/env bash
# Smoke test: install.sh and uninstall.sh both parse and run --dry-run cleanly.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

fail() {
    echo "FAIL: $1" >&2
    exit 1
}

pass() { echo "ok: $1"; }

# install.sh --dry-run
out="$("$REPO_ROOT/scripts/install.sh" --dry-run --binary "$REPO_ROOT/scripts/install.sh" 2>&1)" || \
    fail "install.sh --dry-run exited non-zero: $out"

grep -q "ferry install:" <<<"$out" || fail "install.sh --dry-run missing 'ferry install:' banner"
grep -q "would install" <<<"$out" || fail "install.sh --dry-run missing 'would install' line"
grep -q "/etc/systemd/system/ferry.service" <<<"$out" || fail "install.sh --dry-run missing unit destination"
pass "install.sh --dry-run"

# install.sh --dry-run --config-only (no binary required)
out="$("$REPO_ROOT/scripts/install.sh" --dry-run --config-only 2>&1)" || \
    fail "install.sh --dry-run --config-only exited non-zero: $out"
grep -q "ferry install:" <<<"$out" || fail "install.sh --config-only missing banner"
pass "install.sh --dry-run --config-only"

# uninstall.sh --dry-run
out="$("$REPO_ROOT/scripts/uninstall.sh" --dry-run 2>&1)" || \
    fail "uninstall.sh --dry-run exited non-zero: $out"
grep -q "ferry uninstall:" <<<"$out" || fail "uninstall.sh --dry-run missing banner"
grep -q "would remove" <<<"$out" || fail "uninstall.sh --dry-run missing 'would remove'"
pass "uninstall.sh --dry-run"

# uninstall.sh --dry-run --purge
out="$("$REPO_ROOT/scripts/uninstall.sh" --dry-run --purge 2>&1)" || \
    fail "uninstall.sh --dry-run --purge exited non-zero: $out"
grep -q "purge given" <<<"$out" || fail "uninstall.sh --purge missing 'purge given'"
pass "uninstall.sh --dry-run --purge"

# Validate ferry.service shape.
unit="$REPO_ROOT/systemd/ferry.service"
[[ -f "$unit" ]] || fail "systemd/ferry.service missing"
grep -q "^Description=ferry" "$unit" || fail "unit missing Description"
grep -q "^User=ferry" "$unit" || fail "unit missing User=ferry"
grep -q "^ExecStart=/usr/local/bin/ferry listen" "$unit" || fail "unit missing ExecStart"
grep -q "^ProtectSystem=strict" "$unit" || fail "unit missing ProtectSystem=strict"
grep -q "^ReadWritePaths=/var/lib/ferry /var/log/ferry" "$unit" || fail "unit missing ReadWritePaths"
pass "systemd/ferry.service"

echo "all install/uninstall script tests passed"
