# ferry

Fault-tolerant chunked file uploader.

Designed to ship large files reliably across packet-loss-prone network paths. Single static binary, runs as a systemd service on the receiver, CLI on the sender.

## Status

Pre-release. Wire protocol unstable.

## Building

```sh
./scripts/build.sh   # build the local binary into dist/ferry
./scripts/test.sh    # run tests
./scripts/lint.sh    # gofmt + go vet + golangci-lint
./scripts/fmt.sh     # gofmt + goimports (if installed)
./scripts/cross.sh   # cross-compile all release targets into dist/
./scripts/clean.sh   # remove dist/
```

Open `ferry.code-workspace` in VS Code to pick up the recommended Go editor settings.

## Running the receiver

```sh
ferry listen --config /etc/ferry/config.json
```

The config file is JSON. Defaults are applied for any missing fields:

```json
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
```

A background sweeper runs every `gc_interval_seconds` and removes:
- completed uploads older than `completed_retention_seconds`
- incomplete uploads past `expires_at` (`created_at + incomplete_retention_seconds`)
- orphan `.info` sidecars whose binary is missing
- `.idem` mappings whose target upload is gone

The sweeper takes each upload's lock with a tight deadline, so an in-flight
PATCH always wins.

Tokens live in a separate file (referenced by `tokens_path`) so the main config
can be world-readable while tokens stay `0600`. Each token is stored as a
SHA-256 hex digest; the plaintext is sent by clients via
`Authorization: Bearer <token>`:

```json
{
  "tokens": {
    "<sha256-hex-of-token>": {
      "namespaces": ["alpha", "beta"]
    }
  }
}
```

A namespace value of `"*"` grants access to every namespace.

### Wire protocol

`ferry listen` speaks a tus-1.0.0-compatible subset:

```
POST   /v1/uploads/<namespace>            create upload (Upload-Length, optional Idempotency-Key)
HEAD   /v1/uploads/<namespace>/<id>       report Upload-Offset
PATCH  /v1/uploads/<namespace>/<id>       append bytes at Upload-Offset
DELETE /v1/uploads/<namespace>/<id>       terminate
GET    /health                            healthcheck (no Tus-Resumable required)
```

Per-PATCH bodies are capped at `max_patch_bytes`. Completed uploads are
atomic-renamed from `<id>.partial` to `<id>` so downstream consumers can ignore
in-progress files.

PATCH requests may include `Upload-Checksum: <algo> <hex>` to have the server
hash the bytes as they arrive and 460 + truncate-back on mismatch. Supported
algorithms: `crc32c` (default; clients send this unless `--no-checksum` is
passed) and `sha256`.

## Install on Linux

Build the binary for your target, then run the installer as root:

```sh
./scripts/build.sh
sudo ./scripts/install.sh
sudo $EDITOR /etc/ferry/tokens.json
sudo systemctl restart ferry
```

The installer creates a system `ferry` user, lays down `/etc/ferry/`,
`/var/lib/ferry/data/`, and `/var/log/ferry/` with the right ownership, and
installs and enables the `ferry.service` systemd unit.

Useful flags:

- `--binary <path>` - install a specific binary instead of `dist/ferry-linux-<arch>`
- `--config-only` - just install config + service, skip the binary
- `--dry-run` - print actions without changing anything
- `--prefix <path>` - install the binary under `<prefix>/bin/ferry` (default `/usr/local`)

Uninstall with `sudo ./scripts/uninstall.sh`. Add `--purge` to also delete
config, data, and the `ferry` user.

## License

MIT
