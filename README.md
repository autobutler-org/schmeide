# schmeide

The autobutler-org software factory. A control node and a runner take an assigned, ready-labeled GitHub issue to an opened pull request.

See [docs/design.md](docs/design.md) for the agreed design. The first slice is done. The AI review stage, merging, and pool fallback are later.

## Commands

- `schmeide control` runs the control node: an embedded NATS server with JetStream for the job queue, and an HTTP endpoint at `/webhook` for the schmeide GitHub App.
- `schmeide runner` connects out to the control node, takes one steward's jobs, and opens a pull request for each.
- `schmeide doctor` checks that this machine can run a runner.

Run `schmeide --help` or `schmeide <command> --help` for that command's flags. Every flag has a `SCHMEIDE_<NAME>` environment variable, with hyphens in the flag turned into underscores (`SCHMEIDE_WEBHOOK_SECRET` for `--webhook-secret`). On `control`, a flag wins over the environment, which wins over the `--config` file. On `runner` and `doctor`, a flag wins over the environment. The control node's `--config` file is one `name = value` line per setting.

## Running the first slice

One control node and one runner, on the same machine or not. The target repository needs `.schmeide/config.toml` (below). The schmeide GitHub App supplies the app ID, private key, and webhook secret. Claude Code is the harness, and it must already be installed and logged in on the runner. schmeide does not handle agent API keys.

1. Start the control node. These settings are required: `--webhook-secret`, `--github-app-id`, `--github-app-private-key-file`, `--runner-tokens`, and at least one of `--allowed-orgs` or `--allowed-installations`. `--runner-tokens` is a comma-separated list of `<github login>:<token>`, one entry per steward. Put secrets in the environment or the config file so they stay out of the process list.

   ```sh
   export SCHMEIDE_WEBHOOK_SECRET=...
   export SCHMEIDE_GITHUB_APP_ID=...
   export SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE=/path/to/app.pem
   export SCHMEIDE_RUNNER_TOKENS='octocat:a-token-for-octocat'
   export SCHMEIDE_ALLOWED_ORGS=autobutler-org
   schmeide control
   ```

   The webhook listener is HTTP. It serves `POST /webhook` on `--http-addr` (default `:8080`). GitHub must be able to reach that path. Runners connect to `--nats-addr` (default `127.0.0.1:4222`).

2. Check the runner machine, then start it. `--nats-token` is the token `--runner-tokens` lists for `--steward`. `--nats-url` defaults to `nats://127.0.0.1:4222`.

   ```sh
   schmeide doctor --steward octocat --nats-token a-token-for-octocat
   schmeide runner --steward octocat --nats-token a-token-for-octocat
   ```

   Doctor checks that git is on `PATH`, that a harness is installed and logged in, and that the control node answers for that steward. Each failed check says how to fix it.

3. On an open issue in an allowed repository, assign it to that login and add the label `schmeide:ready` (the `--ready-label` default). An `assigned` or `labeled` delivery creates one job and stores the issue title and body from that delivery. The runner checks out a git worktree on a branch named `<issue number>_<short-description>`, runs a fresh Claude Code session from that stored text, runs the repo's gate, then pushes the branch and opens a pull request as the GitHub App.

A gate failure is commented on the issue without the command's output, and it is not retried. The output stays in the runner's log. Other failures are tried up to `--max-attempts` (default 3).

By default each Claude Code session is started with `--permission-mode acceptEdits`, which only lets it edit files in its worktree. `--claude-args` replaces those flags when a session needs to run commands.

A harness session's environment is `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TZ`, `TMPDIR`, the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and the lowercase forms), `SSL_CERT_FILE`, `SSL_CERT_DIR`, names starting with `LC_` or `XDG_`, names starting with `ANTHROPIC_` or `CLAUDE_`, and any names listed in `--pass-env`. The runner's own `SCHMEIDE_*` settings, including its NATS token, are left out of that session. The gate is not given that list. It inherits the runner process's environment. The session and the gate run as the user that started the runner, so run the runner as a user that holds nothing else worth taking.

## Repo config

A target repo carries its config at `.schmeide/config.toml`. Unknown keys are an error. `gate` is required.

```toml
# Required. Command a job must pass before a PR is opened.
gate = "make check test"
```

See `internal/repoconfig` for the loader. The runner reads this file before the harness session starts.

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
