# schmeide

The autobutler-org software factory. It takes a GitHub issue to a pull request using AI agents.

See [docs/design.md](docs/design.md) for the agreed design.

## Commands

- `schmeide control` runs the control node: an embedded NATS job queue and the GitHub App webhook endpoint.
- `schmeide runner` takes your jobs from the control node and opens a PR for each.
- `schmeide doctor` checks that this machine is ready to run a runner.

Run `schmeide <command> --help` for flags. Every flag can also be set through an environment variable.

## Running the first slice

1. Start the control node with `SCHMEIDE_WEBHOOK_SECRET`, `SCHMEIDE_GITHUB_APP_ID`, and `SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE` set. Set `SCHMEIDE_ALLOWED_ORGS` to accept webhooks only from those orgs.
2. On the runner machine, run `schmeide doctor` and fix anything it reports.
3. Start a runner with `schmeide runner --steward <your github login>`.
4. Assign an issue to the steward and label it `schmeide:ready`. That starts a job.

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
