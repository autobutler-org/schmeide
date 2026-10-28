# schmeide design

This document records the design the team has agreed on so far. It is meant to
give a teammate enough context to pick up an issue. Accepted decisions are in
[Settled decisions](#settled-decisions). The Where column names the code that
implements a decision, or says the code is not updated in this change.
Anything not yet decided is listed under [Open decisions](#open-decisions),
not stated as fact.

This document records decisions. How the code is laid out today is in
[ARCHITECTURE.md](../ARCHITECTURE.md) and [architecture/README.md](architecture/README.md), and what
schmeide does feature by feature is in [user-journeys/README.md](user-journeys/README.md).

## Purpose

schmeide is the software factory for autobutler-org: it takes a GitHub issue to
a reviewed pull request using AI agents, with people stepping in only where the
factory is stuck or a repo asks for a human merge.

It replaces [saddle](https://github.com/brandonapol/saddle), the tool currently
used to build quark. Saddle works for one operator on one laptop. These limits
are the reason for a new tool:

- **State is per laptop.** Tasks, claims, and the merge train live in a local
  SQLite file that is excluded from git, so nothing is shared between people.
- **Task IDs are per machine.** Two operators can produce the same ID, and
  branches with colliding IDs have already appeared on quark's remote.
- **There is no review stage.** CI and the merge queue are the only gate before
  a person presses merge.
- **Stacked PRs are fragile.** Much of saddle's open bug list is about
  restacking and how stacks interact with the merge queue.

## Scope

In scope:

- **Issue to PR.** Agents take an issue and produce a pull request. This is
  what the first slice does.
- **Review and merge.** An AI review stage, and merging once the gates pass.
  Both are built. The factory merges, and the writer fixes review findings,
  only when the repo config's `[automate]` table turns that switch on.

Out of scope for now: planning and intake (turning ideas into issues,
prioritizing, assigning) and keeping repo settings consistent across the org.

## Glossary

| Term | Meaning |
|---|---|
| Steward | The person who owns a job. For a job that comes from a GitHub issue, the steward is the issue assignee. |
| Control node | The single process that receives GitHub webhooks, holds the job queue, and hands out per-job GitHub tokens. |
| Runner | A process on a machine that has an agent harness installed and already logged in. It registers with the control node and pulls jobs. |
| Job | One unit of work: one GitHub issue to be taken to a pull request. |
| Harness | An agent CLI that a runner drives, such as Claude Code or the Grok CLI. |
| Gate | The command a repo requires to pass before a PR is opened, such as `make check test`. |

## Architecture

```mermaid
flowchart LR
    GH[GitHub]
    subgraph CN[Control node]
        WH[Webhook endpoint]
        NATS[Embedded NATS with JetStream]
        WH --> NATS
    end
    R1[Runner: steward A]
    R2[Runner: steward B]

    GH -- issue webhooks --> WH
    R1 -- connects out, pulls jobs --> NATS
    R2 -- connects out, pulls jobs --> NATS
    R1 -- pushes branch, opens PR with per-job token --> GH
    R2 -- pushes branch, opens PR with per-job token --> GH
```

- **One control node binary.** `schmeide control` embeds a NATS server with
  JetStream. A KV bucket records each issue's latest job and whether it finished.
  Runner registration and the latest status of each job are stored on the
  SCHMEIDE_STATUS JetStream stream in the queue's data directory. A restarted
  control node loads them before it accepts new messages. There is one process
  to host. It can be split into separate pieces later if it outgrows that.
- **Webhooks only.** The control node learns about issues from GitHub App
  webhooks. The listener is HTTP and serves `POST /webhook`. It checks
  `X-Hub-Signature-256` against the App's webhook secret. It does not poll
  GitHub.
- **Runners connect out.** A runner opens the connection to the control node,
  so a runner needs no inbound address.
- **Per-job GitHub tokens, per-steward NATS tokens.** A runner has no
  long-lived GitHub credential and no personal GitHub token. See
  [Credentials and the job environment](#credentials-and-the-job-environment).
- **Runners bring their own agent login.** A runner uses the first harness
  whose `Check` passes on its machine. Claude Code and the Grok CLI are both
  built. See [Harnesses](#harnesses). schmeide does not mint or store agent
  keys. A harness session keeps `ANTHROPIC_*`, `CLAUDE_*`, and `XAI_*` from
  the runner's own environment so an existing login still works.

## Job lifecycle

| Step | What happens | When |
|---|---|---|
| 1 | An issue is assigned and marked ready with a label. | Done |
| 2 | GitHub delivers the webhook. The control node creates one job, with the assignee as steward, and stores the issue title and body from that delivery. | Done |
| 3 | A runner belonging to the steward pulls the job. | Done |
| 4 | When the assignee has a registered runner, the job stays on that steward's subject. When the assignee has no registered runner, the job is published on a pool subject. `--pool` defaults off. A runner that sets it registers as a pool runner. Pool runners share one consumer, work one job at a time, and take their own job first. The token, gate, review, and merge path are the same. | Done |
| 5 | The runner creates a git worktree on a new branch and starts a fresh harness session with a brief built from the title and body stored on the job. | Done |
| 6 | The runner runs the repo's gate command. A gate failure is commented on the issue, without the command output, and no PR is opened. | Done |
| 7 | The runner pushes the branch and opens a PR as the schmeide GitHub App. | Done |
| 8 | An AI reviewer checks the PR. With `automate.review_fix = "on"`, the worker fixes what the reviewer finds until the review passes or the round limit is reached. Off, a request for changes leaves the PR open for a person. | Done |
| 9 | After review passes, `automate.merge` absent, empty, or `"off"` leaves the pull request for a person. `"on"` keeps the job in progress until the pull request has merged. It merges with a merge commit once every check run on the head has completed with `success` or `skipped`. `check_wait` (default two minutes) bounds only the wait for the pull request below a stack. When the base branch has a merge queue, green checks add the pull request to the queue, and the job stays in progress until the queue merges it, then reports `merged`. It is not reported `queued`. No check runs is not green, and the job keeps polling. The first failed check runs one writer fix round, when a review round remains, and polling goes on at the new head. A check that is still red, an empty list, and a merge GitHub refuses do not finish the job. A fix round that itself fails comments on the issue, leaves the pull request open, and is not retried. | Done |

Steps 1 to 3 and 5 to 7 shipped in the first slice (#3, #6, and the follow-ups
under [First slice](#first-slice)). Step 8, the AI review stage, has shipped.
Step 9, merging, has shipped. Step 4, pool fallback, has shipped. See
[Routing](#routing).

Before the first job in a new work directory and origin, the runner asks once
on its terminal. A yes is stored under the user config directory, in
`schmeide/trust`. `SCHMEIDE_TRUST=1` lets an unattended run proceed and records
nothing.

A second job for the same repository reuses the bare clone under the work
directory. The gate and the harness session share that repository's Go build
cache and module cache. Removing the work directory removes the cache.

For how a job fails and what is not retried, see
[FAILURE-MODES.md](FAILURE-MODES.md).

## Decisions

### Intake

GitHub issues are the unit of work and the team-visible queue. An issue becomes
a job when it is open, assigned, and carries the ready label, and the webhook
action is `labeled` or `assigned`. Edits and reopens do not create a job and do
not change one that already exists.

### Routing

When the assignee has a live registration, the job stays on that steward's
subject. A registration counts for `RegistrationTTL` (three minutes) after it
was stored, and a running runner publishes it again every
`RegistrationInterval` (one minute), so a runner that stopped falls out of
routing within three minutes. Several runners for one steward share one queue
consumer, so each of that steward's jobs goes to one of them.

When the assignee has no live registration, the control node publishes
the job on a pool subject. `--pool` defaults off. A runner that sets it registers
as a pool runner. Pool runners share one consumer, work one job at a time,
and take their own job first. The token, gate, review, and merge path are the
same. A pool runner spends its owner's agent subscription on someone else's
job, so the flag defaults off.

A runner stays on one job. That is already how the code works, and the code
is not updated in this change.

### Identity

Branches and pull requests are authored by the schmeide GitHub App, not by a
person's account. This keeps people free to review and approve factory PRs, and
it means runners need no personal GitHub token. Commits default to
`schmeide[bot]` / `schmeide[bot]@users.noreply.github.com` (`--git-name` and
`--git-email`) and are made with commit signing turned off, so they are not
signed with the machine owner's key.

### Review

After the gate passes and the pull request is open, a fresh reviewer session
runs in the job's existing worktree, so the checkout and toolchain are already
in place. The reviewer writes its verdict to `.schmeide/review.md`. The runner
reads that file and removes it before any later commit. The runner allows up
to `--max-review-rounds` reviewer sessions (default 3). When the reviewer
requests changes, `automate.review_fix = "on"`, and a round remains, a fresh
worker session applies the findings, the gate runs again, and the same branch
is pushed. With `review_fix` absent or `"off"`, the first request for changes
comments on the issue and leaves the pull request open for a person. A pass
finishes the review. The reviewer is the first ready harness other than the
writer, so Grok reviews Claude Code's change when both are logged in. With one
ready harness, that harness reviews its own change. What happens next is under [Merge](#merge). Exhausting the rounds,
or a gate failure on a fix, comments on the issue and leaves the pull request
open.

### Merge

Each repo chooses in `.schmeide/config.toml` who merges a pull request after
review has passed. The `automate.merge` switch is optional. Absent, empty, and
`"off"` leave the pull request for a person. `"on"` keeps the job in progress
until the pull request has merged. It merges with a merge commit after review
once every check run on the head has completed with `success` or `skipped`.
`check_wait`, an optional Go duration in the same config file (default two
minutes), bounds only the wait for the pull request below a stack. It does
not end this pull request's own wait. A conclusion of `failure`, `cancelled`,
`timed_out`, or empty does not count as success, and a run that has not
completed stays pending. No check runs is not green. None of those finish the
job. When the base branch has a merge queue, green checks add the pull
request to the queue instead of merging it. The job stays in progress until
the queue merges it, then reports `merged`. It is not reported `queued`.
Check runs are read with backoff, from one second doubling to 30 seconds,
and the runner keeps reading.

A factory merge of a stacked pull request, one whose base is another schmeide
pull request's branch, goes bottom first. Before the check wait, the runner
waits up to `check_wait` for the one below to merge. Merging into the branch
below would leave the change off the default branch. While the one below is
open, each poll restacks: the branch is rebased onto the branch below, so the
stack follows a base that moved, and force-pushed with a lease. Once the one
below has merged, which the runner tells by the base it last built on, or the
latest head of the pull request below (`refs/pull/<n>/head`, else its branch),
being in the default branch, it retargets the pull request to the default branch, unless
GitHub already did on deleting the merged branch, and rebases it onto the
default branch. The check wait then runs on the rebased head until this pull
request has merged. A bottom that
does not merge in time, one that closes without merging, or a rebase that
conflicts fails the job, and the comment says which; a person can retarget or
rebase the pull request. Retargeting uses the pulls API's base update from the
runner's own GitHub adapter (`internal/runner/github.go`). A failure after the checks are green says "CI is
green, but" and why, so a pull request that looks ready names why it was not
merged.

Before a factory merge, and before the wait, the runner reads branch protection on the base branch. If it can read it and the branch requires one or more approving reviews, the job fails, the comment names that requirement, the pull request stays open, and nothing is merged or enqueued, because the runner cannot supply that review. A 404 or a 403 on the read does not fail the job: the merge is attempted and GitHub still enforces its rules. If the read fails for any other reason, the job fails, the comment says schmeide could not read branch protection, and the pull request stays open. A canceled wait is not that case. Required status checks are not this failure; the wait covers them. Human merge skips the check.

The first failed check during the wait runs one writer fix round, when a
review round remains. The round counts against `--max-review-rounds`. The
runner gives the session the end of that check's log, runs the gate, pushes the
same branch, and keeps polling on the new head. There is
no second fix round. A check that is still red, and a merge GitHub refuses,
do not finish the job. The runner keeps polling until the pull request has
merged. A fix round that itself fails comments on the issue and leaves the
pull request open. That failure is final and is not retried. The runner reads `[automate]` with the gate, before the harness
session, so the session cannot turn factory merge on for itself
(`internal/repoconfig`, `internal/runner/merge.go`).

### PR shape

One issue is one pull request. The branch name is
`<issue number>_<short-description>`, taken from the issue title stored on
the job. A job whose repository already has a pull request, open or closed,
on a branch starting with `<issue number>_` is treated as already opened.
The check follows pages until that branch is found or a short page comes
back. The match is the `<issue number>_` prefix, including after a retitle,
and it counts open and closed pull requests.

Stacked pull requests have shipped (#58). The dependency is GitHub's
issue-dependencies blocked-by list, not a key in `.schmeide/config.toml`.
The worktree is created from the base branch, and the pull request opens
against that same branch. No open blocker with an open pull request whose
head branch starts with `<issue number>_` means the base is the repository
default branch. The prefix is that blocker's issue number. A blocker that
is not open, and an open blocker with no such pull request, do not count.
Exactly one such open blocker selects that pull request's head branch as
the base. Two or more such open blockers have to be one linear stack: the
base is the top, the one whose chain of pull request bases down to the
default branch holds every other one, whatever order GitHub lists them in.
Blockers that are not one chain, such as two pull requests side by side on
the default branch (a diamond), fail the job finally. The runner comments on
the issue that they are not one linear stack, does not open a pull request,
and does not retry (`internal/runner/github.go`, `internal/runner/runner.go`). Restacking when
the base moves or the bottom merges is under the factory merge above.

### Harnesses

Harnesses are pluggable. The runner drives a harness through an adapter
interface that states what every harness must support. Claude Code and the
Grok CLI are both implementations (`internal/harness`). The Grok adapter
shells out to the Grok CLI, the same shape as the Claude Code adapter, with
no new module dependency.

`All()` offers Claude Code first and the Grok CLI second. The runner uses
the first harness whose `Check` passes, so a working Claude Code is still
selected. The runner offers that list, and `--claude-args` applies only to
Claude Code (`internal/runner/cli.go`, `internal/runner/runner.go`). A
missing or logged-out harness is skipped when another one is ready. Doctor
fails the harness check only when every harness fails (`internal/doctor`).

Every session starts from a permission floor. `--claude-args` and
`--grok-args` both default to `--permission-mode acceptEdits`. The runner
refuses to start when either contains `--dangerously-skip-permissions` or a
`--permission-mode` other than `acceptEdits`, unless
`--allow-dangerous-harness-args` is set. An OS sandbox is not part of this
floor. See [harness permissions](permissions.md).

### Isolation

Each job runs in its own git worktree on the runner's host, using the machine's
installed toolchains. The job flow is written against a `Workspace` interface
so a container backend can replace the worktree later. Container isolation is
not built. It is aimed at pooled work.

What a job's processes may see is under
[Credentials and the job environment](#credentials-and-the-job-environment).
The session and the gate run as the operating-system user that started the
runner.

### Repo config

Each repo the factory works on carries its own schmeide config, checked in and
reviewed like code. The file is `.schmeide/config.toml`. `gate` is required:
the command that must pass before a PR is opened. The `[automate]` table is
optional and holds `"on"`/`"off"` switches that default to off: `merge`, which
[Merge](#merge) describes, `review_fix`, which [Review](#review) describes,
and `create_issues`, which is accepted ahead of the feature and read by
nothing yet. Unknown keys are an error. `gate_timeout` and `session_timeout` are optional Go durations. Absent
or empty keeps the runner flag: `--gate-timeout` defaults to 30 minutes and
`--session-timeout` defaults to 2 hours. A positive value overrides that flag
for the repo. An unparseable or non-positive value is a config error that
names the key and gives `30m` as an example. When a deadline fires, the
runner cancels that process group, fails the job, and comments on the issue
that the session or the gate timed out. The comment does not include the
command's output. The runner reads the file before the harness session, so
the session cannot change the gate it is about to be held to, cannot turn
factory merge on for itself, and cannot lengthen its own deadline. Other
settings, such as reviewer prompts, are added to the schema when the feature
that reads them is built. A pull request
dependency is GitHub's issue-dependencies blocked-by list, not a key in
this file. See [PR shape](#pr-shape).

### Credentials and the job environment

A runner stores no long-lived GitHub credential. When it takes a job it asks
the control node on `schmeide.token.<steward>`, with the job's subject as the
request. The control node loads that job from the stream and mints a GitHub
App installation token limited to the job's repository and to `contents:
write`, `pull_requests: write`, `issues: write`, `checks: read`,
`administration: read`, `actions: read`, `workflows: write`, and
`merge_queues: write` (`internal/control/token.go`). The request is refused when the subject is not
one of that steward's jobs. GitHub expires an installation token after one
hour; the runner revokes it with `DELETE /installation/token` when the delivery
ends. Git receives the token as an `http.extraHeader` in that git command's
environment, so the token is not written into git config.

NATS access is per steward. `--runner-tokens` is a comma-separated list of
`<login>:<token>` entries. A runner connects as its steward, with that token,
and can only create that steward's consumer, pull and acknowledge that
steward's jobs, publish that steward's registration and statuses, and request
tokens on that steward's token subject. Replies use an inbox prefix for that
steward, so one steward cannot read another's token replies. The control
node's own NATS connection uses a generated password
(`internal/control/queue.go`).

`internal/runner/env.go` names what a job may see. The base list is `PATH`,
`HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TZ`, `TMPDIR`, the proxy
variables (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and the lowercase forms),
`SSL_CERT_FILE`, `SSL_CERT_DIR`, and any name starting with `LC_` or `XDG_`.
A harness session sees the base list, any name passed in `--pass-env`, and
names starting with `ANTHROPIC_`, `CLAUDE_`, or `XAI_`. The session keeps
those prefixes from the runner's own environment so an existing login still
works. schmeide does not mint or store agent keys. The runner starts the
session with that environment, so the session does not see the runner's
`SCHMEIDE_*` settings, including its NATS token.

The gate sees the base list and any name passed in `--gate-pass-env`, and
never a `SCHMEIDE_*` name: the runner refuses one in `--gate-pass-env`, and
the filter drops it anyway. `HOME` and `TMPDIR` point at a scratch directory
of the job's own under `--work-dir` (`scratch/<job>`), outside the checkout,
and `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, and
`XDG_STATE_HOME` are dropped so those default under it too. Closing the
worktree removes that directory. The gate also gets the repository's shared
Go caches (`GOCACHE`, `GOMODCACHE`), so `go` and `make` still work with
`PATH` and the certificate variables from the base list.

Git gets its own environment: the worktree adds the installation token to
it, and the runner gives it nothing else (`internal/runner/runner.go`), so
the token never reaches the gate or the session. The gate's output is not
posted to the issue. The comment names the command and the failure, and the
output tail stays in the runner's log.

### Deployment

schmeide is deployment-agnostic. The control node is a plain binary configured
by flags, environment variables, or a file, and nothing in the code, config
schema, or defaults may assume a particular host.

There are two requirements wherever it runs:

- GitHub must be able to reach the control node's webhook endpoint, `POST
  /webhook` on `--http-addr`. The listener is HTTP.
- Runners must be able to reach the control node's NATS address.

How to run a control node, with one reference pattern, is in
[DEPLOYMENT.md](DEPLOYMENT.md). Any host-specific setup belongs there or in the
operator's own infrastructure, not in the design of the tool. A control node
without a public address needs a tunnel for webhooks.

`schmeide version` prints the version the Makefile baked in; an unstamped build
prints `dev`. When a runner registers, the control node compares versions. If
both are valid semver and differ, it logs a warning and still accepts the
runner. See [VERSIONING.md](VERSIONING.md).

## First slice

Epic #7 is done. One control node and one runner take an assigned,
ready-labeled issue to an opened pull request, using Claude Code.

| Issue | Piece | Depends on |
|---|---|---|
| #3 | Control node with embedded NATS and webhook intake | |
| #4 | Harness adapter interface with the Claude Code implementation | |
| #5 | Tracked per-repo config with the gate command | |
| #6 | Runner that takes a job from issue to opened PR | #3, #4, #5 |

Follow-ups that landed with the slice:

- **#14.** The control node subscribes to runner registration and job status.
  Both are stored on the SCHMEIDE_STATUS JetStream stream in the queue's data
  directory. A restarted control node loads them before it accepts new
  messages.
- **#15.** A job that fails for a reason other than the gate is tried again, up
  to `--max-attempts` (default 3). The runner counts those work failures in the
  persisted job status, and a token hand-back, shutdown, or stuck-job hand-back
  does not consume an attempt. A restarted runner restores the attempt count.
  A job whose stage record names an open pull request resumes at review
  and does not open a second one. A job with no pull request starts over. The wait before the next try is 30 seconds and has no flag. A gate
  failure is final. So are a missing or invalid `.schmeide/config.toml` and
  open blockers side by side (#212): a retry cannot change them. A pull request on a branch named `<issue number>_*`
  suppresses a second PR.
- **#103.** A runner sends a JetStream `InProgress` heartbeat every third of
  `--ack-wait` (default 5 minutes), so a dead runner's unacknowledged job is
  redelivered. `--stuck-after` (default 3 hours) also cancels and Naks a job
  that reports no state change within the budget, preventing a live but wedged
  runner from heartbeating it forever.
- **#16.** Webhooks are limited to an allowlist of orgs or App installations.
- **Pull request #30.** Runner credentials are scoped per steward, the job
  carries the issue text from the event that created it, the harness session
  runs with the reduced environment above, and the allowlist is required. An
  empty allowlist allows no job. A job queued before that change has no issue
  text; the runner fails it and says to remove the ready label and add it
  again.

The schmeide GitHub App is a prerequisite for a live delivery. Tests use saved
webhook payloads, a fake GitHub API, and a fake harness.

The AI review stage shipped after this slice. See [Review](#review). Merging
shipped after this slice. See [Merge](#merge). The Grok CLI harness shipped
after this slice. See [Harnesses](#harnesses). Pool fallback shipped after
this slice. See [Routing](#routing). Stacked pull requests shipped after
this slice. See [PR shape](#pr-shape).

Not in the first slice, and not shipped:

- Container isolation for jobs
- Polling GitHub for issues
- More than one job at a time on a runner. Accepted as no change: a runner stays on one job, which is already how the code works.

## Settled decisions

| Decision | Outcome | Where |
|---|---|---|
| Name of the ready label | `schmeide:ready`. Override with `--ready-label`. | `internal/control/intake.go` (`DefaultReadyLabel`) |
| Path and format of the per-repo config file | `.schmeide/config.toml`. `gate` is required and must be a non-empty command. `[automate]` is optional: its `merge`, `review_fix`, and `create_issues` switches are `"on"` or `"off"`, and absent, empty, and blank mean `"off"`. `gate_timeout` and `session_timeout` are optional Go durations. Absent or empty means the runner flag, which defaults to 30 minutes for the gate and 2 hours for a session. A positive value overrides that flag. An unparseable or non-positive value is an error that names the key and gives `30m` as an example. Unknown keys are an error. | `internal/repoconfig`, `internal/runner` |
| Which orgs or App installations may create jobs | Required. `--allowed-orgs` (owner logins, compared case-insensitively) or `--allowed-installations` (numeric installation IDs), or both. A job is accepted when it matches either list. `schmeide control` refuses to start when both are empty, and an empty allowlist allows no job. A delivery that would create a job and matches neither list is rejected with 403. A delivery that does not describe a ready job is ignored, including one from outside the allowlist. A bad signature is rejected before that check. | `internal/control/intake.go`, `internal/control/cli.go` |
| Shape of the job message | JSON with `repo`, `repo_id`, `issue`, `steward`, `installation_id`, `title`, and `body`, on `schmeide.jobs.<steward>.<repo id>.<issue>` when the assignee has a registered runner, and on a pool subject when the assignee has none. Created only for an open issue that has an assignee and the ready label, and only from an `issues` action of `labeled` or `assigned`. The steward is `issue.assignee`, or the first of `issue.assignees` when that is empty. One job per repository id and issue number; a repeat delivery is a duplicate and does not replace the stored text. The runner does not fetch the issue. | `internal/control/intake.go`, `internal/control/queue.go` |
| How the runner is verified | Automated tests. The runner tests stand up the embedded queue, a fake GitHub REST API, and a fake harness. Intake tests use saved webhook payloads. | `internal/runner/runner_test.go`, `internal/harness/fake.go`, `internal/control/testdata` |
| What a runner may authenticate as | A per-steward NATS token from `--runner-tokens`. A per-job GitHub App installation token scoped to that repository and to contents, pull requests, issues, workflows, and merge queues write, and checks, administration, and actions read. No long-lived GitHub credential on the runner. | `internal/control/queue.go`, `internal/control/token.go` |
| What a harness session may see | The base list in `internal/runner/env.go`, plus `ANTHROPIC_*`, `CLAUDE_*`, and `XAI_*` kept from the runner's own environment so an existing login still works, plus `--pass-env`. schmeide does not mint or store agent keys. Not the runner's `SCHMEIDE_*` settings. | `internal/runner/env.go`, `internal/runner/runner.go` |
| What the gate may see (#111) | The base list plus `--gate-pass-env`, never a `SCHMEIDE_*` name, with `HOME` and `TMPDIR` at a per-job scratch directory that closing the worktree removes. It no longer inherits the runner process environment. | `internal/runner/env.go`, `internal/runner/workspace.go` |
| Pool opt-in (#31) | `--pool` defaults off. A runner that sets it registers as a pool runner. When the assignee has a registered runner, the job stays on that steward's subject. When the assignee has no registered runner, the job is published on a pool subject. Pool runners share one consumer, work one job at a time, and take their own job first. The token, gate, review, and merge path are the same. | `internal/runner/cli.go`, `internal/control/queue.go`, `internal/runner/runner.go` |
| Merge default (#32, #197, #259) | `automate.merge` absent, empty, or `"off"` leaves the pull request for a person. `"on"` keeps the job in progress until the pull request has merged. It merges with a merge commit after review once every check run on the head has completed with `success` or `skipped`. `failure`, `cancelled`, `timed_out`, an empty conclusion, and a run that has not completed do not count as success. `check_wait` (default two minutes) bounds only the wait for the pull request below a stack. When the base branch has a merge queue, green checks add the pull request to the queue, and the job stays in progress until the queue merges it, then reports `merged`. It is not reported `queued`. If branch protection on the base branch is readable and requires one or more approving reviews, the job fails before the wait, the pull request stays open, and nothing is merged or enqueued; a 404 or 403 on the read does not fail it, and any other read failure fails the job and leaves the pull request open. No check runs is not green, and the job keeps polling. The first failed check runs one writer fix round, when a review round remains, and polling goes on at the new head. A check that is still red, an empty list, and a merge GitHub refuses do not finish the job. A fix round that itself fails comments on the issue, leaves the pull request open, and is not retried. Checks are read with backoff from one second up to 30 seconds. A stacked pull request merges bottom first: it waits up to `check_wait` for the one below to merge, rebasing onto that branch as it moves, then is retargeted to the default branch and rebased onto it, and a failure after green checks says why it did not merge. | `internal/repoconfig`, `internal/runner/merge.go` |
| Dependency declaration (#34) | Shipped (#58). The dependency is GitHub's issue-dependencies blocked-by list, not a key in `.schmeide/config.toml`. The worktree is created from the base branch, and the pull request opens against that same branch. No open blocker with an open pull request whose head branch starts with `<issue number>_` means the base is the repository default branch. The prefix is that blocker's issue number. A blocker that is not open, and an open blocker with no such pull request, do not count. Exactly one such open blocker selects that pull request's head branch as the base. Two or more such open blockers have to be one linear stack, and the top of it is the base (#94); blockers that are not one chain fail the job, and the runner comments that they are not one linear stack and does not open a pull request. | `internal/runner/github.go`, `internal/runner/runner.go` |
| Second harness (#33) | Shell out to the Grok CLI, the same shape as the Claude Code adapter, with no new module dependency. `All()` offers Claude Code first and the Grok CLI second. The runner uses the first harness whose `Check` passes, so a working Claude Code is still selected. A missing or logged-out harness is skipped when another one is ready. Doctor fails the harness check only when every harness fails. `--claude-args` applies only to Claude Code. | `internal/harness`, `internal/doctor`, `internal/runner/cli.go`, `internal/runner/runner.go` |
| Concurrent jobs (#44) | A runner stays on one job. Accepted as no change. Already how the code works. | The code is not updated in this change. Already how the runner works. |

## Open decisions

- Which runtime isolates a pool job, whether that isolation applies only to
  pool jobs, and what image the job runs in. Proposed in #59 and not accepted,
  so container isolation stays unbuilt.

## Later

Roughly in the order they are expected to matter. None of these have been built:

1. Container isolation for jobs
