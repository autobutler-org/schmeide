# How a job fails

This page describes what the runner does today when a job goes wrong, and what
it does not do. It covers only behavior that is built. For the lifecycle see
[design.md](design.md#job-lifecycle).

## Attempts

A runner writes the number of failed work outcomes on every job status. The
status stream is stored in the control node's data directory, so a restarted
runner restores the count before it claims the job. A restart does not grant a
job a fresh retry budget. A new job for the same issue does: the control node
stores a `pending` status with no count when it creates the job.

## Pending status

When the control node creates a job it stores a `pending` status for it, so
`schmeide status` lists the job before a runner claims it (JN-CLI-016).

- If that status cannot be stored, the job is still created and the control
  node logs `could not record the job as pending; schmeide status will not list
  it until a runner claims it`. The job appears once a runner reports
  `claimed`.
- If the job cannot be stored, the webhook fails as before and the `pending`
  status is deleted, so no line is left for a job that does not exist.
- A job a runner hands back keeps the runner's last state in status until the
  next runner reports on it.

- `--max-attempts` (default 3) is how many failed work outcomes the runners
  allow for a job. While the count is below that, the runner reports the job
  as retrying and hands it back to the queue. The wait before the next try is
  30 seconds and has no flag.
- When the count reaches the limit, the failure is final: the runner comments
  on the issue and acknowledges the job.
- A token hand-back does not consume an attempt. If the runner cannot get a
  GitHub token for the job, it hands the job back without commenting, because
  the issue could not be reached anyway. An App without one of the permissions
  the control node asks for (Contents, Pull requests, Issues, Checks,
  Administration, Actions, Workflows, Merge queues) gets no token, so every
  job is handed back until the App and its installation have them.
- A job that outlives its token gets a new one. GitHub expires an installation
  token an hour after it is minted. Before each GitHub call and git command the
  runner checks the token it holds, and when that token is within five minutes
  of expiry it asks the control node for another one for the same job. If that
  request fails, the call that needed the token fails with "refresh the job's
  GitHub token: ..." and the job fails like any other GitHub error (an attempt
  is used). The outcome comment is still attempted with its own refresh; if
  that fails too, the runner logs `msg="could not comment on the issue"` with
  the refresh error, nothing reaches the issue, and `schmeide status` still
  shows the job's final state.
- A shutdown does not consume an attempt. If the runner is stopping, it hands
  the job back at once for another runner.
- A gate failure is final. It is the session's own result, so it is not
  retried.
- A missing or invalid `.schmeide/config.toml` is final. The runner reads it
  from the base branch before any session, and a retry reads the same file.
  The comment names the error from `internal/repoconfig`. Push a fixed config
  and add the ready label again to make a new job.
- Open blockers side by side, whose pull requests are not one linear stack,
  are final. The blocked-by list does not change between attempts. The
  comment says "are not one linear stack, and schmeide stacks only on a single
  chain of blockers".
- A review that is not passed after the allowed rounds is final. A fix whose
  gate fails is a gate failure and is final too.
- A factory merge that does not land is final. The pull request already
  exists, and a retry would treat the job as done. That includes a check that
  is still red after its one fix round (see below).
- A harness session that fails, times out, or makes no changes counts as a
  failed attempt. So do a git error and a GitHub error before the pull request
  is open.

A non-interactive run with no recorded decision for the work directory and
origin, and without `SCHMEIDE_TRUST=1`, refuses the job before it creates a
worktree. That refusal is a failed work outcome, so the count above includes
it. The persisted counter does not treat it specially.

A pull request already open on a branch starting with `<issue number>_`, open
or closed, keeps a retried job from opening a second one.

After a runner restart, a job whose stage record names an open pull request
resumes at review and does not open a second pull request. A job with no pull
request starts over.

## Deadlines

A harness session is canceled after `--session-timeout`, default 2 hours. The
gate command is canceled after `--gate-timeout`, default 30 minutes.

The repo config can set `session_timeout` and `gate_timeout` in
`.schmeide/config.toml`. Each is a Go duration such as `30m`. Absent or empty
keeps the runner flag. A positive value overrides it for that repo. A value
that does not parse, or is not positive, is a config error. The runner reads
the file before the session starts, so the session cannot lengthen its own
deadline.

When a deadline fires the runner cancels that process group and comments on the
issue that the session or the gate timed out. The comment does not include the
command's output.

- A gate timeout is final, like any gate failure.
- A session timeout before the pull request is open counts as a failed
  attempt.

### A session that goes silent

A harness session, worker or reviewer, is also canceled when its output has
been silent for 15 minutes (`sessionIdleLimit` in `internal/runner/runner.go`),
however much of the session timeout is left. This catches a session that
stalls after its first API request and would otherwise sit until the two hour
default. Fifteen minutes sits above the ten minutes one Bash tool call may run
without the session printing anything. The limit has no flag or config key.

`Runner.runHarness` wraps `Request.Output` in a writer that pushes the limit
back on every write, and cancels the session's context when the limit passes.
The harness kills the session's process group as it does for a timeout. The
attempt fails with "the claude-code session produced no output for 15m0s" (or
"the grok reviewer session produced no output for 15m0s" for a reviewer), and
the job is handled exactly as a session timeout:

- Before the pull request is open it counts as a failed attempt and is retried
  up to `--max-attempts`. The retrying status detail is "attempt 1 of 3 failed:
  the claude-code session produced no output for 15m0s".
- After the last attempt the issue gets "schmeide could not finish this job and
  did not open a pull request:" followed by that text. No session output is in
  the comment.
- In a reviewer or fix session after the pull request is open it is final and
  the pull request stays open.

Claude Code runs with `--output-format stream-json --verbose`, so the runner's
stderr shows each event of the session as it happens, including the last one
before it went silent.

A gate that fails without timing out also keeps its output out of the issue
comment. The comment names the command and the failure, and the output tail is
in the runner's log.

### A runner started in a terminal

A runner run from an interactive terminal passes its stderr, that terminal, to
every harness session as output. Each harness session and gate command starts
in its own process session (`Setsid` in `internal/harness/claude.go`,
`internal/harness/grok.go`, and `internal/runner/workspace.go`), so it has no
controlling terminal and `SIGTTOU` or `SIGTTIN` cannot stop it. Before this,
a session was only a background process group on the runner's terminal: the
CLI's first terminal ioctl stopped it (`ps` state `T`) and it sat until the
session deadline fired and the job failed with "the claude-code session timed
out". A session stopped that way now points at a build from before the fix.

The fix shipped in v0.0.3 (#242). Runners older than that started each
session with `Setpgid` instead of `Setsid`. The usual trigger was a shell the
session started that ran the operator's shell startup files: a `stty` call
there (prezto's runs one) touched the terminal, `SIGTTOU` stopped the
session's whole process group, and the job sat until the 2 hour
`--session-timeout`. Run `schmeide version` on the runner machine; anything
older than v0.0.3 (or a `dev` build from before #242) needs an upgrade. No
flag or config key works around it on an old runner.

## The gate fails on every job because of the runner's environment

The gate does not run where the operator's shell runs. `worktree.Run` in
`internal/runner/workspace.go` runs it with `sh -c` in a worktree that `git
worktree add` created fresh for the job, with this environment:

- `gateEnv` in `internal/runner/env.go`: only the base names (`PATH`, `HOME`,
  `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TZ`, `TMPDIR`, the
  `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` variables in both cases,
  `SSL_CERT_FILE`, `SSL_CERT_DIR`), every `LC_*` and `XDG_*` name, and the
  names in `--gate-pass-env`. No `SCHMEIDE_*` name, ever.
- `scratchHome` in the same file then drops `HOME`, `TMPDIR`,
  `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, and `XDG_STATE_HOME`,
  and sets `HOME` and `TMPDIR` to `<work-dir>/scratch/<repo_id>-<issue>`.
  That directory is emptied when the job starts and removed when it ends.
- `goCacheEnv` in `internal/runner/cache.go` adds `GOCACHE`, `GOMODCACHE`,
  and `GOFLAGS` pointing at `<work-dir>/cache/<repo_id>`. Only Go gets a
  cache that outlives the job.

So anything a toolchain keeps in the home directory or in untracked files is
missing, even though the same command passes in the operator's own checkout:

- Resolved packages that are not committed: Dart's `.dart_tool/` in the
  checkout and `~/.pub-cache`, a `node_modules/`, a Python virtualenv, and
  the like.
- SDK and tool caches under `~/.cache`, `~/.config`, or other dotfiles.
- Environment variables the shell profile sets (`PUB_CACHE`, `JAVA_HOME`, a
  version manager's shims) when they are not on the base list or in
  `--gate-pass-env`.

A toolchain on `PATH` that is older than the version the repo pins fails the
same way. That is the runner machine's state, not the change's.

### What it looks like

Every job fails at the gate within minutes, whatever the issue asks for. The
gate is never retried (see [Attempts](#attempts)), so each job fails once and
the issue gets, from `Runner.gateOutcome` in `internal/runner/runner.go`:

```text
schmeide did not open a pull request: the gate command `<gate>` failed: exit status 2. The gate's output is in the runner's log.
```

The exit status is whatever the gate returned; `make` returns 2. When the
gate fails in a review fix round after the pull request is open, the comment
starts "schmeide left the pull request open:" and ends with the pull request
URL. The output never reaches the issue. The runner logs
`level=WARN msg="gate failed"` with the last 4000 bytes of it in `output`.

Running schmeide against a Flutter repo whose gate was `make check test` gave
two examples in that log line:

- No `.dart_tool/` in the fresh worktree and no `~/.pub-cache` in the scratch
  `HOME`, so `dart format` could not read the repo's lint settings and
  formatted with its defaults. `check/format/flutter` logged `Failed to
  resolve package URI "package:flutter_lints/flutter.yaml"` and `Formatted
  1894 files (131 changed)`, and failed.
- The runner machine's Flutter SDK was 3.47.7 and the pubspec pinned 3.47.8:
  `Because quark requires Flutter SDK version 3.47.8, version solving
  failed.`

### Diagnose

Run the gate on unmodified `main` in a fresh clone, with the environment
`gateEnv` and `scratchHome` build. Add each name the runner's
`--gate-pass-env` lists, and any `LC_*`, `XDG_*`, `TZ`, proxy, or
`SSL_CERT_*` variable the runner has (but not the four `XDG_*_HOME` names
`scratchHome` drops):

```sh
repo=$(mktemp -d) scratch=$(mktemp -d)
git clone -q --depth 1 https://github.com/<owner>/<repo> "$repo"
cd "$repo"
env -i PATH="$PATH" USER="$USER" LOGNAME="$LOGNAME" SHELL="$SHELL" \
  TERM="$TERM" LANG="$LANG" HOME="$scratch" TMPDIR="$scratch" \
  sh -c '<the gate from .schmeide/config.toml>'
```

If that fails, the environment is the cause, not the sessions' changes. If it
passes in the operator's normal shell but fails here, the difference between
the two environments is the missing piece.

### Fix

- **Make the gate resolve its own dependencies.** For example `gate =
  "flutter pub get && make check test"`, or a `check` target in the
  Makefile that depends on a target running `flutter pub get` (`npm ci`,
  `uv sync`, and so on for other toolchains). This also makes the gate pass
  in any other fresh clone, such as CI.
- **Pass cache locations with `--gate-pass-env`** (or
  `SCHMEIDE_GATE_PASS_ENV`), so the gate reuses a cache instead of
  downloading everything into a scratch `HOME` every job. For example set
  `PUB_CACHE=$HOME/.pub-cache` in the runner's environment and start it with
  `--gate-pass-env PUB_CACHE`. Listing `HOME`, `TMPDIR`, or one of the
  `XDG_*_HOME` names does nothing, because `scratchHome` replaces or drops
  them after the list is applied. Never list a secret. See
  [config.md](config.md#runner-flags).
- **Keep the runner machine's SDKs at the versions the repo pins.** Upgrade
  the SDK on `PATH` of the user that runs `schmeide runner` when a repo
  raises its pin.

Relabel each failed issue after the fix (JN-PR-005). See JN-FAIL-024.

## Sessions cannot run commands under the default permission floor

Both `--claude-args` and `--grok-args` default to `--permission-mode
acceptEdits` ([permissions.md](permissions.md)). `ClaudeCode.Run` in
`internal/harness/claude.go` starts `claude --print --output-format
stream-json --verbose --permission-mode acceptEdits`. In `--print` mode no one
can answer a permission prompt, so Claude Code refuses each Bash tool call,
and the session's own output (on the runner's stderr) shows tool results
saying "This command requires approval". The session can edit files but
cannot run the repo's formatter, linter, or tests. Its mistakes only surface
when the gate runs, which fails the job (see above) with no fix round before
the pull request is open.

This default is deliberate: a session that may run commands can do anything
the runner's OS user can. To opt in, widen the session flags and accept that
risk explicitly:

```sh
schmeide runner --steward <login> \
  --claude-args "--permission-mode auto" --allow-dangerous-harness-args
```

or `SCHMEIDE_CLAUDE_ARGS="--permission-mode auto"` with
`SCHMEIDE_ALLOW_DANGEROUS_HARNESS_ARGS=true`. Without
`--allow-dangerous-harness-args` the runner refuses to start with
`--claude-args contains "--permission-mode auto", which lets a session skip
its permission checks; remove it, or pass --allow-dangerous-harness-args to
accept that (see docs/permissions.md)`. Read
[permissions.md](permissions.md#opting-in) before turning it on, and run the
runner as a user that holds nothing else worth taking. See JN-FAIL-025.

## Registration refresh

A runner re-publishes its registration every minute while `Run` runs
(`Runner.keepRegistered` in `internal/runner/runner.go`). A publish that fails
logs `could not report to the control node` with the subject and is retried on
the next tick. A runner that cannot reach the control node for three minutes
(`control.RegistrationTTL`) stops counting as registered: its steward's next
job goes to the pool and `schmeide status` stops listing it, until a refresh
gets through again. Nothing is commented on any issue.

## Delivery heartbeats and stuck jobs

`--ack-wait` defaults to 5 minutes. It is the JetStream delivery lease: while
a runner owns a job it sends `InProgress` every third of that duration. The
runner therefore has to remain connected to the control node throughout a
long session. If the process dies or heartbeats stop, JetStream redelivers the
job after the lease expires.

The lease belongs to the shared consumer, not to each runner. The first runner
to start for a steward (or the first `--pool` runner) creates the consumer with
its `--ack-wait`, and later runners use it unchanged. A runner whose
`--ack-wait` differs logs `a shared job consumer already has another ack wait;
heartbeating at the consumer's` and heartbeats every third of the consumer's
lease, so a mismatch cannot cause a redelivery of a job that is still being
worked. To change the lease, delete the consumer (`runner-<steward>` or
`runner-_pool` on `SCHMEIDE_JOBS`) with the `nats` CLI while no runner holds a
job from it, then start a runner with the new `--ack-wait`.

JetStream redelivers without limit: no `MaxDeliver` is set. The runner's
attempt count is what ends retries, because it does not count hand-backs for a
missing token, for shutdown, or for a dead runner, and a job stopped by
`MaxDeliver` would end with no status and no issue comment.

`--stuck-after` defaults to 3 hours and is a separate no-progress budget. Each
reported job-state change resets it. If the budget expires, the runner cancels
the work, stops heartbeats, and Naks the delivery so another runner can take
it immediately. This hand-back does not consume an attempt. Set the budget
longer than the longest legitimate phase; in particular, account for
`--session-timeout`, `--gate-timeout`, and slow factory-merge checks.

## Secret scan before push

Before `git push`, the runner scans the lines the job's branch adds relative to
its base for credential-shaped literals: PEM private keys, GitHub tokens
(including `ghs_` installation tokens), GitLab tokens, AWS access keys, JWTs,
Anthropic API keys, and xAI API keys. It also looks for the job's installation
token and the runner's NATS token verbatim. If it finds one, it refuses the push
and `git push` does not run. A literal that is already on the base is not the
job's and does not block the push.

The error names each file, the line, and the kind, for example
`refusing to push: config.go:12 contains a GitHub token`. It does not include
the secret. A line containing `schmeide:allow-secret` is skipped, except when
it holds one of the runner's own tokens verbatim, which is reported as a
`runner credential`. The scan reads git output only: it does not look at the
process environment.

A refused push is a failed work outcome, so it counts as an attempt.

## Token revocation

When a delivery ends, the runner revokes the installation token the delivery
holds with `DELETE /installation/token`. A token a refresh replaced earlier is
not revoked; it was within five minutes of its expiry. That happens after a final state, a retry, a
stuck-job drop, and a shutdown hand-back. If GitHub refuses the call or cannot
be reached within 30 seconds, the runner logs
`could not revoke the job's token; it expires within the hour` and nothing else
changes: the job's state, its issue comment, and its acknowledgment stand, and
the token stays valid until GitHub expires it.

## Branches

A job worktree can move only its own `<number>_*` branch, where the number is
the job's issue. A git hook in the worktree refuses to create, move, or delete
any other `<digits>_*` branch. That stops one job's session from touching
another job's branch. Other branch names are not covered by the hook.

## One job at a time

A runner works one job at a time. There is no concurrency setting.

After review passes:

- With `automate.merge` absent, empty, or `"off"`, the runner leaves the pull request
  open. A person merges it.
- With `automate.merge = "on"`, the runner keeps reading the head commit's
  check runs, with backoff from one second up to 30 seconds, until the pull
  request has merged. `check_wait` does not end that wait. A completed check
  whose conclusion is `skipped` counts as success. A conclusion of `failure`,
  `cancelled`, `timed_out`, or empty does not. No check runs is not green.
  A red check, an empty list, and a merge GitHub refuses leave the job in
  progress and the pull request open. A stacked pull request first waits
  for the one below it to merge, for up to `check_wait`, rebasing onto that
  branch as it moves, then is retargeted to the default branch and rebased
  onto it; a bottom that does not merge in time, closes unmerged, or a rebase
  that conflicts leaves it open with a comment saying which. The job then
  waits for this pull request to merge the same way.
- Before the wait, the runner reads branch protection on the base branch. If
  it can read it and the branch requires one or more approving reviews, the
  job fails, the comment names that requirement, the pull request stays open,
  and nothing is merged or enqueued. The runner cannot supply that review. A
  404 or a 403 on the read does not fail the job: the merge is attempted and
  GitHub still enforces its rules. If the read fails for any other reason, the job fails, the comment says schmeide could not read branch protection, and the pull request stays open.
  A canceled wait is not that case. Required status checks are not this
  failure. Human merge skips the check.
- When the base branch has a merge queue, green checks add the pull request to
  the queue instead of merging it. The job stays in progress until the queue
  merges it, then reports `merged`. It is not reported `queued`.
- The first red check runs one writer fix round, when a review round remains.
  The round counts against `--max-review-rounds`. Polling then continues on
  the new head. There is no second fix round. A fix round that itself fails
  comments on the issue and leaves the pull request open. That failure is
  final. A check that is still red does not.

## Not built

Container isolation is not built. A job runs in a git worktree on the runner's
host, as the operating-system user that started the runner, with the machine's
installed toolchains.
