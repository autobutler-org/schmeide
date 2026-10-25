# schmeide glossary

The terms schmeide's docs and flags use, in alphabetical order. For the full
design see [design.md](design.md).

## blocked-by

GitHub's issue-dependencies list, which schmeide reads to stack pull requests.
It is not a key in `.schmeide/config.toml`. Open blockers that have an open
pull request on a branch starting with `<issue number>_` make the top of their
stack the base for the new pull request. Otherwise the base is the repository
default branch. Two or more such blockers have to form one linear stack, each
pull request based on the one below it; blockers side by side (a diamond) fail
the job finally.

## restack

Rebasing a stacked pull request's branch so it follows its base. While the pull
request below is open, a factory merge's stack wait rebases onto that branch at
each poll. Once the one below merges, the runner retargets the pull request to
the default branch, if GitHub has not, and rebases it onto the default branch.
The rebased branch is force-pushed with a lease. A conflict leaves the branch
as it was and fails the job, asking a person to rebase.

## check_wait

An optional key in `.schmeide/config.toml` that sets how long a factory merge
waits for every check run on the pull request head to succeed. It is a Go
duration and defaults to two minutes. The wait covers the fix round's new head.
An unparseable or non-positive value is a config error. It only matters when
`automate.merge = "on"`.

## control node

The single `schmeide control` process. It receives GitHub webhooks, holds the
job queue in an embedded NATS server with JetStream, and mints a GitHub
installation token for each job a runner asks for.

## factory merge

The `automate.merge = "on"` setting in `.schmeide/config.toml`. After review passes,
the runner waits `check_wait` for every check run on the head to succeed. The
first red check runs one fix round when a review round remains, then polling
continues on the new head inside the same wait. There is no second fix round.
A check that is still red, or a wait that ends without success, leaves the pull
request open. Green checks merge the pull request with a merge commit, or, when
the base branch has a merge queue, add it to the queue and the job is reported
queued. If branch protection can be read and requires approving reviews, the
job fails before the wait and nothing is merged or enqueued. A 404 or 403 on
that read does not fail the job. No check runs does not count as green. Absent,
empty, and `"off"` leave the merge to a person.

## fix round

The one writer session a factory merge runs when the first check in the wait is
red and a review round remains.

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
pull requests, comment, read checks, read branch protection, and read Actions
job logs. GitHub expires it after one hour, so
a runner asks the control node for a new one for the same job when the one it
holds is within five minutes of expiry. Runners hold no long-lived GitHub
credential.

## pending

The job state the control node records when it creates a job, before any
runner has claimed it. `schmeide status` lists a pending job with how long it
has waited. Not final: the runner's `claimed` replaces it. Not to be confused
with [queued](#queued), which is a finished job in a merge queue.

## pool

The fallback queue for a job whose assignee has never registered a runner.
Registrations are kept in the control node's data directory and never expire,
so a steward whose runner is offline still gets jobs on their own subject. A runner
started with `--pool` (off by default) also takes pool jobs. Pool runners work
one job at a time and take their own steward's job first.

## queued

The finished job state when a factory merge added the pull request to the base
branch's merge queue. It is not merged yet. Status does not keep listing it.

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
`--max-attempts`, and that count is stored with job status, so a restarted
runner continues from the last count. After a restart, a job whose stage
record names an open pull request resumes at review and does not open a second
pull request. A job with no pull request starts over.

## steward

The person who owns a job. For a job that comes from a GitHub issue, the steward
is the issue assignee, and the runner that works the job is one of that
person's.

## worktree

The git worktree a runner creates for each job, on its own branch, using the
host's installed toolchains. The harness session, the gate, and review all run
in it. Container isolation is not built.
