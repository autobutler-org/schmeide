# schmeide

The autobutler-org software factory. A control node and a runner take an assigned, ready-labeled GitHub issue to an opened pull request, then review the change. The default is that a person presses merge.

See [docs/design.md](docs/design.md) for the agreed design. The first slice, the AI review stage, merging, and the Grok CLI harness are done. Pool fallback is later.

## Commands

- `schmeide control` runs the control node: an embedded NATS server with JetStream for the job queue, and an HTTP endpoint at `/webhook` for the schmeide GitHub App.
- `schmeide runner` connects out to the control node, takes one steward's jobs, opens a pull request for each, and reviews the change in the same worktree. The factory merges that pull request only when the repo config sets `merge = "factory"`. The default is a person.
- `schmeide doctor` checks that this machine can run a runner.

Run `schmeide --help` or `schmeide <command> --help` for that command's flags. Every flag has a `SCHMEIDE_<NAME>` environment variable, with hyphens in the flag turned into underscores (`SCHMEIDE_WEBHOOK_SECRET` for `--webhook-secret`). On `control`, a flag wins over the environment, which wins over the `--config` file. On `runner` and `doctor`, a flag wins over the environment. The control node's `--config` file is one `name = value` line per setting.

## Running the first slice

One control node and one runner, on the same machine or not. The target repository needs `.schmeide/config.toml` (below). The schmeide GitHub App supplies the app ID, private key, and webhook secret. Claude Code and the Grok CLI are the harnesses. The runner uses the first one whose check passes. Claude Code is offered first, so Claude Code wins when it is usable. At least one of them must already be installed and logged in on the runner. A missing or logged-out harness is skipped when another one is ready. schmeide does not handle agent API keys.

1. Start the control node. These settings are required: `--webhook-secret`, `--github-app-id`, `--github-app-private-key-file`, `--runner-tokens`, and at least one of `--allowed-orgs` or `--allowed-installations`. `--runner-tokens` is a comma-separated list of `<github login>:<token>`, one entry per steward. Put secrets in the environment or the config file so they stay out of the process list.

   ```sh
   export SCHMEIDE_WEBHOOK_SECRET=...
   export SCHMEIDE_GITHUB_APP_ID=...
   export SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE=/path/to/app.pem
   export SCHMEIDE_RUNNER_TOKENS='octocat:a-token-for-octocat'
   export SCHMEIDE_ALLOWED_ORGS=autobutler-org
   schmeide control
   ```

   The webhook listener is plain HTTP and must sit behind a TLS-terminating proxy. It serves `POST /webhook` on `--http-addr` (default `:8080`). GitHub must be able to reach that path. `--webhook-rate` (default `1000`) is the maximum number of webhook requests accepted per minute. Further requests are rejected and publish no job. `--delivery-ttl` (default `10m`) is how long an `X-Github-Delivery` id is remembered. A repeat within that time is acknowledged and is not published again. Runners connect to `--nats-addr` (default `127.0.0.1:4222`).

2. Check the runner machine, then start it. `--nats-token` is the token `--runner-tokens` lists for `--steward`. `--nats-url` defaults to `nats://127.0.0.1:4222`.

   ```sh
   schmeide doctor --steward octocat --nats-token a-token-for-octocat
   schmeide runner --steward octocat --nats-token a-token-for-octocat
   ```

   Doctor checks that git is on `PATH`, that at least one harness is installed and logged in, and that the control node answers for that steward. It fails the harness check only when every harness fails. Each failed check says how to fix it.

3. On an open issue in an allowed repository, assign it to that login and add the label `schmeide:ready` (the `--ready-label` default). An `assigned` or `labeled` delivery creates one job and stores the issue title and body from that delivery. The runner checks out a git worktree on a branch named `<issue number>_<short-description>`, runs a fresh harness session from that stored text, runs the repo's gate, then pushes the branch and opens a pull request as the GitHub App. Claude Code runs that session when it is usable.

After the gate passes and the pull request is open, the runner runs up to `--max-review-rounds` reviewer sessions (default 3) in that worktree. Each round is a fresh reviewer session. The reviewer writes `.schmeide/review.md` there. The runner reads that file and removes it before any later commit. A pass finishes the review. Absent, empty, and `human` leave the pull request for a person. `factory` merges with a merge commit after review only when every check run on the head has succeeded within two minutes. No check runs is not green. A failed check or a wait that ends without success comments on the issue, leaves the pull request open, and is not retried. A request for changes, when a round remains, runs a fresh worker session, the gate, and a push of the same branch. Exhausting the rounds, or a gate failure on a fix, comments on the issue and leaves the pull request open.

A gate failure is commented on the issue without the command's output, and it is not retried. The output stays in the runner's log. A failure before the pull request is open, other than a gate failure, is tried up to `--max-attempts` (default 3).

By default each harness session is started with `--permission-mode acceptEdits`, which only lets it edit files in its worktree. `--claude-args` replaces those flags for Claude Code when a session needs to run commands. It does not apply to the Grok CLI.

A harness session's environment is `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TZ`, `TMPDIR`, the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and the lowercase forms), `SSL_CERT_FILE`, `SSL_CERT_DIR`, names starting with `LC_` or `XDG_`, names starting with `ANTHROPIC_` or `CLAUDE_`, and any names listed in `--pass-env`. The runner's own `SCHMEIDE_*` settings, including its NATS token, are left out of that session. The gate is not given that list. It inherits the runner process's environment. The session and the gate run as the user that started the runner, so run the runner as a user that holds nothing else worth taking.

## Repo config

A target repo carries its config at `.schmeide/config.toml`. Unknown keys are an error. `gate` is required. `merge` is optional.

```toml
# Required. Command a job must pass before a PR is opened.
gate = "make check test"

# Optional. Who merges after review has passed and CI is green.
# Absent, empty, and "human" leave the pull request for a person.
# "factory" means the factory merges.
merge = "human"
```

See `internal/repoconfig` for the loader. The runner reads this file before the harness session starts, so the session cannot change the gate or turn factory merge on.

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
