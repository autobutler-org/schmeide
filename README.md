# schmeide

The autobutler-org software factory. A control node and a runner take an assigned, ready-labeled GitHub issue to an opened pull request, then review the change. The default is that a person presses merge.

New here? Follow the [quickstart](docs/QUICKSTART.md) from zero to one pull request. See [docs/design.md](docs/design.md) for the agreed design. The first slice, the AI review stage, merging, the Grok CLI harness, pool fallback, and stacked pull requests are done. `--pool` defaults off, and a runner that sets it registers as a pool runner. When the assignee has a registered runner, the job stays on that steward's subject. When the assignee has no registered runner, the job is published on a pool subject. Pool runners share one consumer, work one job at a time, and take their own job first. The token, gate, review, and merge path are the same. Stacked pull requests have shipped (#58). The dependency is GitHub's issue-dependencies blocked-by list, not a key in `.schmeide/config.toml`. The worktree is created from the base branch, and the pull request opens against that same branch. No open blocker with an open pull request whose head branch starts with `<issue number>_` means the base is the repository default branch. The prefix is that blocker's issue number. A blocker that is not open, and an open blocker with no such pull request, do not count. Exactly one such open blocker selects that pull request's head branch as the base. Two or more such open blockers fail the job. The runner comments on the issue that only one open dependency is supported and does not open a pull request. Container isolation is not built.

## Commands

- `schmeide control` runs the control node: an embedded NATS server with JetStream for the job queue, and an HTTP endpoint at `/webhook` for the schmeide GitHub App.
- `schmeide runner` connects out to the control node, takes one steward's jobs, opens a pull request for each, and reviews the change in the same worktree. With `--pool`, which defaults off, it registers as a pool runner and can take a job whose assignee has no registered runner. It still works one job at a time and takes its own job first. The factory merges that pull request only when the repo config sets `merge = "factory"`. The default is a person.
- `schmeide doctor` checks that this machine can run a runner.
- `schmeide status` shows registered runners and queued or running jobs.
- `schmeide init` writes a repo config and a control node config in this directory.
- `schmeide completion` prints a shell completion script for bash, zsh, or fish.

Run `schmeide --help` or `schmeide <command> --help` for that command's flags. Every flag has a `SCHMEIDE_<NAME>` environment variable, with hyphens in the flag turned into underscores (`SCHMEIDE_WEBHOOK_SECRET` for `--webhook-secret`). On `control`, a flag wins over the environment, which wins over the `--config` file. On `runner` and `doctor`, a flag wins over the environment. The control node's `--config` file is one `name = value` line per setting. Terms are defined in [docs/GLOSSARY.md](docs/GLOSSARY.md), and `schmeide --help` links there.

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

A gate failure is commented on the issue without the command's output, and it is not retried. The output stays in the runner's log. A harness session stops after `--session-timeout` (default 2 hours). The gate stops after `--gate-timeout` (default 30 minutes). Either deadline cancels that process group, fails the job, and comments on the issue that the session or the gate timed out, without the command's output. The repo config keys `session_timeout` and `gate_timeout` override the flags. An empty value keeps the flag. A gate timeout is not retried. A session timeout before the pull request is open counts toward `--max-attempts`. A failure before the pull request is open, other than a gate failure, is tried up to `--max-attempts` (default 3). The count is kept in memory, and a restarted runner starts the attempt count over.

By default each harness session is started with `--permission-mode acceptEdits`, which only lets it edit files in its worktree. `--claude-args` replaces those flags for Claude Code when a session needs to run commands. It does not apply to the Grok CLI.

A harness session's environment is `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TZ`, `TMPDIR`, the proxy variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and the lowercase forms), `SSL_CERT_FILE`, `SSL_CERT_DIR`, names starting with `LC_` or `XDG_`, names starting with `ANTHROPIC_`, `CLAUDE_`, or `XAI_`, and any names listed in `--pass-env`. The session keeps those prefixes from the runner's own environment so an existing login still works. schmeide does not mint or store agent keys. The runner's own `SCHMEIDE_*` settings, including its NATS token, are left out of that session. The gate is not given that list. It inherits the runner process's environment. The session and the gate run as the user that started the runner, so run the runner as a user that holds nothing else worth taking.

## Repo config

A target repo carries its config at `.schmeide/config.toml`. Unknown keys are an error. `gate` is required. `merge` is optional.

```toml
# Required. Command a job must pass before a PR is opened.
gate = "make check test"

# Optional. Who merges after review has passed and CI is green.
# Absent, empty, and "human" leave the pull request for a person.
# "factory" means the factory merges.
merge = "human"

# Optional. How long the gate command may run. A Go duration.
# Absent or empty keeps the runner's --gate-timeout (default 30m).
gate_timeout = "30m"

# Optional. How long one harness session may run. A Go duration.
# Absent or empty keeps the runner's --session-timeout (default 2h).
session_timeout = "2h"
```

The full key list is in [docs/config.md](docs/config.md). See `internal/repoconfig` for the loader. The runner reads this file before the harness session starts, so the session cannot change the gate or turn factory merge on.

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
