# How a job fails

This page describes what the runner does today when a job goes wrong, and what
it does not do. It covers only behavior that is built. For the lifecycle see
[design.md](design.md#job-lifecycle).

## Attempts

A runner counts failed work outcomes per job, in memory. The count is not
written anywhere. **If the runner process restarts, the count starts over at
zero.** A job that failed twice before a restart gets a full set of attempts
again.

- `--max-attempts` (default 3) is how many failed work outcomes this process
  allows for a job. While the count is below that, the runner reports the job
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
it. The in-memory counter does not treat it specially.

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

- With `merge` absent, empty, or `human`, the runner leaves the pull request
  open. A person merges it.
- With `merge = "factory"`, the runner waits for the head commit's check runs
  to succeed, then merges with a merge commit. The wait is `check_wait` from the
  repo config, default two minutes. No check runs is not green.
- Before the wait, the runner reads branch protection on the base branch. If
  it can read it and the branch requires one or more approving reviews, the
  job fails, the comment names that requirement, the pull request stays open,
  and nothing is merged or enqueued. The runner cannot supply that review. A
  404 or a 403 on the read does not fail the job: the merge is attempted and
  GitHub still enforces its rules. Required status checks are not this
  failure. Human merge skips the check.
- When the base branch has a merge queue, green checks add the pull request to
  the queue instead of merging it. The job is reported merged once it is
  queued.
- The first red check during the wait runs one writer fix round, when a review
  round remains. The round counts against `--max-review-rounds`. Polling then
  continues on the new head inside the same wait. There is no second fix round.
- A check that is still red, or a wait that ends without success, comments on
  the issue and leaves the pull request open. That failure is final.

## Not built

Container isolation is not built. A job runs in a git worktree on the runner's
host, as the operating-system user that started the runner, with the machine's
installed toolchains.
