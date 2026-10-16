# schmeide glossary

The terms schmeide's docs and flags use, in alphabetical order. For the full
design see [design.md](design.md).

## blocked-by

GitHub's issue-dependencies list, which schmeide reads to stack pull requests.
It is not a key in `.schmeide/config.toml`. Exactly one open blocker that has an
open pull request on a branch starting with `<issue number>_` makes that
branch the base for the new pull request. Otherwise the base is the repository
default branch. Two or more such blockers fail the job.

## check_wait

An optional key in `.schmeide/config.toml` that sets how long a factory merge
waits for every check run on the pull request head to succeed. It is a Go
duration and defaults to two minutes. An unparseable or non-positive value is
a config error. It only matters when `merge = "factory"`.

## control node

The single `schmeide control` process. It receives GitHub webhooks, holds the
job queue in an embedded NATS server with JetStream, and mints a GitHub
installation token for each job a runner asks for.

## factory merge

The `merge = "factory"` setting in `.schmeide/config.toml`. After review passes,
the runner merges the pull request with a merge commit, but only when every
check run on the head has succeeded within the `check_wait` window. No check
runs does not count as green. Absent, empty, and `human` leave the merge to a
person.

## gate

The command a repo's `.schmeide/config.toml` requires to pass before a pull
request is opened, such as `make check test`. A gate failure is final: the
runner comments on the issue without the command's output and does not retry.

## gate timeout

The longest the gate may run. The `gate_timeout` config key overrides the
runner's `--gate-timeout` flag, which defaults to 30 minutes. When it fires,
the runner cancels the gate's process group, fails the job, and does not retry.

## installation token

A short-lived GitHub App token the control node mints for one job. It is limited
to that job's repository and to the permissions a runner needs to push, open
pull requests, comment, and read checks. GitHub expires it after one hour.
Runners hold no long-lived GitHub credential.

## pool

The fallback queue for a job whose assignee has no registered runner. A runner
started with `--pool` (off by default) also takes pool jobs. Pool runners work
one job at a time and take their own steward's job first.

## ready label

The label that marks an assigned issue as ready for the factory. It is
`schmeide:ready` unless `--ready-label` says otherwise. An open, assigned issue
that gets this label, or that is assigned while carrying it, becomes a job.

## runner

A process on a machine with an agent harness already installed and logged in.
It connects out to the control node, takes its steward's jobs one at a time,
and takes each job to a pull request.

## session timeout

The longest one harness session may run. The `session_timeout` config key
overrides the runner's `--session-timeout` flag, which defaults to two hours.
When it fires, the runner cancels the session, fails the job, and comments on
the issue. A timeout before the pull request is open counts toward
`--max-attempts`, and that count is kept in memory, so a restarted runner starts
over.

## steward

The person who owns a job. For a job that comes from a GitHub issue, the steward
is the issue assignee, and the runner that works the job is one of that
person's.

## worktree

The git worktree a runner creates for each job, on its own branch, using the
host's installed toolchains. The harness session, the gate, and review all run
in it. Container isolation is not built.
