# schmeide

The autobutler-org software factory. The design is in progress, so the binary only prints usage for now.

See [docs/design.md](docs/design.md) for the agreed design and the first slice of work.

## Repo config

A target repo carries its config at `.schmeide/config.toml`. Unknown keys are an error.

```toml
# Required. Command a job must pass before a PR is opened.
gate = "make check test"
```

See `internal/repoconfig` for the loader.

## Development

Requires Go and `make`; the Go toolchain version pinned in `go.mod` is downloaded automatically.

```sh
make build   # build ./build/schmeide
make test    # run tests
make check   # lint (golangci-lint, applies autofixes)
make format  # format
make help    # list all targets
```

Seeded from [exokomodo/template-golang](https://github.com/exokomodo/template-golang).
