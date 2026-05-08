#!/usr/bin/env bash
# Production installer for ferry on Linux.
#
# Idempotent: safe to re-run. Re-execs under sudo if not already root.
#
# Usage:
#   ./scripts/install.sh                    # install from dist/ferry-linux-<arch>
#   ./scripts/install.sh --binary <path>    # install a specific binary
#   ./scripts/install.sh --config-only      # skip binary, just config + service
#   ./scripts/install.sh --dry-run          # print actions, change nothing
#   ./scripts/install.sh --prefix /opt      # install binary under /opt/bin
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY=""
CONFIG_ONLY=0
DRY_RUN=0
PREFIX="/usr/local"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --binary)
            BINARY="${2:-}"
            shift 2
            ;;
        --config-only)
            CONFIG_ONLY=1
            shift
            ;;
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        --prefix)
            PREFIX="${2:-}"
            shift 2
            ;;
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

# Re-exec under sudo if not root. Skip when --dry-run so users can preview without privileges.
if [[ $DRY_RUN -eq 0 && $(id -u) -ne 0 ]]; then
    echo "ferry install: re-executing under sudo"
    args=()
    [[ -n "$BINARY" ]] && args+=(--binary "$BINARY")
    [[ $CONFIG_ONLY -eq 1 ]] && args+=(--config-only)
    args+=(--prefix "$PREFIX")
    # Use an absolute shell path and let sudo reset PATH to a sane default.
    # Trusting the caller's PATH when escalating to root is a known foot-gun:
    # a writable directory earlier in PATH could shadow `bash` or downstream
    # tools (`useradd`, `systemctl`, ...) with attacker-controlled binaries.
    exec sudo /bin/bash "$0" "${args[@]}"
fi

# Detect target arch.
case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *)
        echo "unsupported arch: $(uname -m)" >&2
        exit 1
        ;;
esac

# Resolve the binary path unless --config-only.
if [[ $CONFIG_ONLY -eq 0 ]]; then
    if [[ -z "$BINARY" ]]; then
        # Try $REPO_ROOT/dist/ferry-linux-<arch>, then fall back to dist/ferry.
        for candidate in \
            "$REPO_ROOT/dist/ferry-linux-$ARCH" \
            "$REPO_ROOT/dist/ferry"; do
            if [[ -f "$candidate" ]]; then
                BINARY="$candidate"
                break
            fi
        done
    fi
    if [[ -z "$BINARY" || ! -f "$BINARY" ]]; then
        echo "ferry binary not found. Build with ./scripts/build.sh or pass --binary <path>." >&2
        exit 1
    fi
fi

run() {
    if [[ $DRY_RUN -eq 1 ]]; then
        echo "would run: $*"
    else
        "$@"
    fi
}

write_file() {
    # write_file <path> <mode> <owner> <<<content
    local path="$1" mode="$2" owner="$3"
    local content
    content="$(cat)"
    if [[ $DRY_RUN -eq 1 ]]; then
        echo "would write $path (mode=$mode owner=$owner, $(wc -c <<<"$content") bytes)"
        return
    fi
    install -m "$mode" -o "${owner%:*}" -g "${owner#*:}" /dev/null "$path"
    printf '%s' "$content" >"$path"
}

ensure_user() {
    if id ferry >/dev/null 2>&1; then
        echo "user ferry already exists"
        return
    fi
    run useradd --system --no-create-home --shell /usr/sbin/nologin ferry
}

ensure_dir() {
    # ensure_dir <path> <mode> <owner>
    #
    # Creates the directory (with the given mode + owner) if missing. If it
    # already exists we leave its mode/ownership alone - we don't want to
    # silently chown system dirs like /usr/local/bin that we just happen to
    # cross during install.
    local path="$1" mode="$2" owner="$3"
    if [[ -d "$path" ]]; then
        return
    fi
    run install -d -m "$mode" -o "${owner%:*}" -g "${owner#*:}" "$path"
}

# enforce_dir is for ferry-owned directories where we DO want to converge
# to the target mode + owner on every run (e.g. /etc/ferry, /var/lib/ferry).
enforce_dir() {
    local path="$1" mode="$2" owner="$3"
    if [[ -d "$path" ]]; then
        run chown "$owner" "$path"
        run chmod "$mode" "$path"
    else
        run install -d -m "$mode" -o "${owner%:*}" -g "${owner#*:}" "$path"
    fi
}

echo "ferry install: arch=$ARCH dry_run=$DRY_RUN config_only=$CONFIG_ONLY prefix=$PREFIX"

ensure_user

# Resolve the final binary location regardless of whether we're installing
# the binary or just the service. The systemd unit's ExecStart is templated
# to point at this path, so a non-default --prefix produces a unit that
# matches.
BIN_DST="$PREFIX/bin/ferry"

if [[ $CONFIG_ONLY -eq 0 ]]; then
    echo "ferry install: would install $BINARY -> $BIN_DST"
    ensure_dir "$PREFIX/bin" 0755 root:root
    run install -m 0755 -o root -g root "$BINARY" "$BIN_DST"
fi

enforce_dir /etc/ferry           0750 root:ferry
enforce_dir /var/lib/ferry       0750 ferry:ferry
enforce_dir /var/lib/ferry/data  0750 ferry:ferry
enforce_dir /var/log/ferry       0750 ferry:ferry

# Default config: only written when missing.
if [[ ! -f /etc/ferry/config.json ]]; then
    echo "ferry install: would write default /etc/ferry/config.json"
    write_file /etc/ferry/config.json 0640 root:ferry <<'JSON'
{
  "listen_addr": "0.0.0.0:7421",
  "data_dir": "/var/lib/ferry/data",
  "tokens_path": "/etc/ferry/tokens.json",
  "completed_retention_seconds": 86400,
  "incomplete_retention_seconds": 604800,
  "gc_interval_seconds": 3600,
  "max_patch_bytes": 67108864,
  "disk_safety_margin_bytes": 1073741824
}
JSON
else
    echo "config /etc/ferry/config.json already present, leaving alone"
fi

if [[ ! -f /etc/ferry/tokens.json ]]; then
    echo "ferry install: would write placeholder /etc/ferry/tokens.json"
    write_file /etc/ferry/tokens.json 0640 root:ferry <<'JSON'
{
  "tokens": {}
}
JSON
    echo
    echo "  >>> EDIT /etc/ferry/tokens.json TO ADD TOKENS BEFORE FERRY WILL ACCEPT UPLOADS <<<"
    echo
else
    echo "tokens /etc/ferry/tokens.json already present, leaving alone"
fi

# Install the systemd unit. The unit template ships with a @@FERRY_BIN@@
# placeholder so a non-default --prefix produces a working ExecStart.
UNIT_SRC="$REPO_ROOT/systemd/ferry.service"
UNIT_DST="/etc/systemd/system/ferry.service"
if [[ ! -f "$UNIT_SRC" ]]; then
    echo "missing $UNIT_SRC; expected systemd/ferry.service in repo" >&2
    exit 1
fi
echo "ferry install: would install $UNIT_SRC -> $UNIT_DST (ExecStart=$BIN_DST listen ...)"
if [[ $DRY_RUN -eq 1 ]]; then
    echo "would substitute @@FERRY_BIN@@ -> $BIN_DST in $UNIT_DST"
else
    # sed escape: BIN_DST may contain '/'; use '|' as the delimiter.
    sed "s|@@FERRY_BIN@@|$BIN_DST|g" "$UNIT_SRC" >"$UNIT_DST.tmp"
    install -m 0644 -o root -g root "$UNIT_DST.tmp" "$UNIT_DST"
    rm -f "$UNIT_DST.tmp"
fi

run systemctl daemon-reload

# Only enable + (re)start the service when we know the binary at the unit's
# ExecStart path is in place. Cases where it isn't:
#  - `--config-only` was passed and the binary was never copied here
#  - real install but the binary was missing for some other reason
# In either case starting now would just fail and leave the installer
# non-zero after partial setup; admin can re-run once the binary is there.
should_start=1
if [[ $CONFIG_ONLY -eq 1 ]]; then
    should_start=0
elif [[ $DRY_RUN -eq 0 && ! -x "$BIN_DST" ]]; then
    should_start=0
fi

if [[ $should_start -eq 1 ]]; then
    run systemctl enable ferry
    if [[ $DRY_RUN -eq 0 ]] && systemctl is-active --quiet ferry; then
        run systemctl restart ferry
    else
        run systemctl start ferry
    fi
else
    echo "ferry install: skipping systemctl enable/start (binary not present at $BIN_DST or --config-only set; re-run installer once the binary is in place)"
fi

echo
echo "ferry installed."
echo "  status:  systemctl status ferry"
echo "  logs:    journalctl -u ferry -f"
echo "  tokens:  \$EDITOR /etc/ferry/tokens.json   # then: systemctl restart ferry"
