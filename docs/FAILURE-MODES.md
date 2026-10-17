# How a job fails

This page describes what the runner does today when a job goes wrong, and what
it does not do. It covers only behavior that is built. For the lifecycle see
[design.md](design.md#job-lifecycle).

## Attempts

A runner writes the number of failed work outcomes on every job status. The
status stream is stored in the control node's data directory, so a restarted
runner restores the count before it claims the job. A restart does not grant a
job a fresh retry budget.

- `--max-attempts` (default 3) is how many failed work outcomes the runners
  allow for a job. While the count is below that, the runner reports the job
  as retrying and hands it back to the queue. The wait before the next try is
  30 seconds and has no flag.
- When the count reaches the limit, the failure is final: the runner comments
  on the issue and acknowledges the job.
- A token hand-back does not consume an attempt. If the runner cannot get a
  GitHub token for the job, it hands the job back without commenting, because
  the issue could not be reached anyway.
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

A gate that fails without timing out also keeps its output out of the issue
comment. The comment names the command and the failure, and the output tail is
in the runner's log.

## Delivery heartbeats and stuck jobs

`--ack-wait` defaults to 5 minutes. It is the JetStream delivery lease: while
a runner owns a job it sends `InProgress` every third of that duration. The
runner therefore has to remain connected to the control node throughout a
long session. If the process dies or heartbeats stop, JetStream redelivers the
job after the lease expires.

`--stuck-after` defaults to 3 hours and is a separate no-progress budget. Each
reported job-state change resets it. If the budget expires, the runner cancels
the work, stops heartbeats, and Naks the delivery so another runner can take
it immediately. This hand-back does not consume an attempt. Set the budget
longer than the longest legitimate phase; in particular, account for
`--session-timeout`, `--gate-timeout`, and slow factory-merge checks.

## Secret scan before push

Before `git push`, the runner scans the tracked files in the job's worktree for
credential-shaped literals: PEM private keys, GitHub tokens, GitLab tokens, AWS
access keys, and JWTs. If it finds one, it refuses the push and `git push`
does not run.

The error names each file, the line, and the kind, for example
`refusing to push: config.go:12 contains a GitHub token`. It does not include
the secret. A line containing `schmeide:allow-secret` is skipped. The scan
reads files only: it does not look at the process environment.

A refused push is a failed work outcome, so it counts as an attempt.

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
- With `automate.merge = "on"`, the runner waits for the head commit's check runs
  to succeed, then merges with a merge commit. The wait is `check_wait` from the
  repo config, default two minutes, read with backoff from one second up to
  30 seconds. No check runs is not green. A stacked pull request first waits
  for the one below it to merge, rebasing onto that branch as it moves, then
  is retargeted to the default branch and rebased onto it; a bottom that does
  not merge in time, closes unmerged, or a rebase that conflicts leaves it open
  with a comment saying which.
- Before the wait, the runner reads branch protection on the base branch. If
  it can read it and the branch requires one or more approving reviews, the
  job fails, the comment names that requirement, the pull request stays open,
  and nothing is merged or enqueued. The runner cannot supply that review. A
  404 or a 403 on the read does not fail the job: the merge is attempted and
  GitHub still enforces its rules. If the read fails for any other reason, the job fails, the comment says schmeide could not read branch protection, and the pull request stays open.
  A canceled wait is not that case. Required status checks are not this
  failure. Human merge skips the check.
- When the base branch has a merge queue, green checks add the pull request to
  the queue instead of merging it. The job is reported queued.
- The first red check during the wait runs one writer fix round, when a review
  round remains. The round counts against `--max-review-rounds`. Polling then
  continues on the new head inside the same wait. There is no second fix round.
- A check that is still red, or a wait that ends without success, comments on
  the issue and leaves the pull request open. That failure is final.

## Not built

Container isolation is not built. A job runs in a git worktree on the runner's
host, as the operating-system user that started the runner, with the machine's
installed toolchains.
