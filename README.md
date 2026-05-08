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

## License

MIT
