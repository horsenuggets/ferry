# ferry

Fault-tolerant chunked file uploader.

Designed to ship large files reliably across packet-loss-prone network paths. Single static binary, runs as a systemd service on the receiver, CLI on the sender.

## Status

Pre-release. Wire protocol unstable.

## Building

```sh
make build       # build the local binary
make test        # run tests
make lint        # run golangci-lint
make cross       # cross-compile all release targets into dist/
```

## License

MIT
