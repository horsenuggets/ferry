#!/usr/bin/env bash
# Uninstaller for ferry on Linux. Idempotent.
#
# By default removes the binary, the unit file, and disables the service.
# Config + data + the ferry user are kept unless --purge is given.
#
# Usage:
#   ./scripts/uninstall.sh           # safe uninstall, keep data
#   ./scripts/uninstall.sh --purge   # also remove /etc/ferry, /var/lib/ferry, /var/log/ferry, ferry user
#   ./scripts/uninstall.sh --dry-run # print actions, change nothing
set -euo pipefail

PURGE=0
DRY_RUN=0
PREFIX="/usr/local"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --purge) PURGE=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        --prefix) PREFIX="${2:-}"; shift 2 ;;
        -h|--help)
            sed -n '2,11p' "$0"
            exit 0
            ;;
        *)
            echo "unknown flag: $1" >&2
            exit 2
            ;;
    esac
done

if [[ $DRY_RUN -eq 0 && $(id -u) -ne 0 ]]; then
    echo "ferry uninstall: re-executing under sudo"
    args=()
    [[ $PURGE -eq 1 ]] && args+=(--purge)
    args+=(--prefix "$PREFIX")
    exec sudo -E env PATH="$PATH" bash "$0" "${args[@]}"
fi

run() {
    if [[ $DRY_RUN -eq 1 ]]; then
        echo "would run: $*"
    else
        "$@" || true
    fi
}

echo "ferry uninstall: dry_run=$DRY_RUN purge=$PURGE prefix=$PREFIX"

# Stop + disable. Both ignore "not running / not enabled" errors.
run systemctl stop ferry
run systemctl disable ferry

UNIT_DST="/etc/systemd/system/ferry.service"
if [[ -f "$UNIT_DST" || $DRY_RUN -eq 1 ]]; then
    echo "ferry uninstall: would remove $UNIT_DST"
    run rm -f "$UNIT_DST"
fi
run systemctl daemon-reload

BIN_DST="$PREFIX/bin/ferry"
if [[ -f "$BIN_DST" || $DRY_RUN -eq 1 ]]; then
    echo "ferry uninstall: would remove $BIN_DST"
    run rm -f "$BIN_DST"
fi

if [[ $PURGE -eq 1 ]]; then
    echo "ferry uninstall: --purge given, removing config + data + user"
    for d in /etc/ferry /var/lib/ferry /var/log/ferry; do
        echo "ferry uninstall: would remove $d"
        run rm -rf "$d"
    done
    if id ferry >/dev/null 2>&1; then
        run userdel ferry
    fi
else
    echo
    echo "config kept at /etc/ferry, data kept at /var/lib/ferry, logs at /var/log/ferry."
    echo "re-run with --purge to remove them."
fi

echo "ferry uninstall: done."
