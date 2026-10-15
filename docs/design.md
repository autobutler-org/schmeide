# schmeide design

This document records the design the team has agreed on so far. It is meant to
give a teammate enough context to pick up an issue. Decisions the code has
settled are in [Settled decisions](#settled-decisions). Anything not yet
decided is listed under [Open decisions](#open-decisions), not stated as fact.

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
  The review stage is built. Merging is not.

Out of scope for now: planning and intake (turning ideas into issues,
prioritizing, assigning) and keeping repo settings consistent across the org.

## Glossary

| Term | Meaning |
|---|---|
| Steward | The person who owns a job. For a job that comes from a GitHub issue, the steward is the issue assignee. |
| Control node | The single process that receives GitHub webhooks, holds the job queue, and hands out per-job GitHub tokens. |
| Runner | A process on a machine that has an agent harness installed and already logged in. It registers with the control node and pulls jobs. |
| Job | One unit of work: one GitHub issue to be taken to a pull request. |
| Harness | An agent CLI that a runner drives, such as Claude Code. |
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
  JetStream. The job stream is the record of which issues already have a job.
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
- **Runners bring their own agent login.** A runner uses the harness that is
  already installed and logged in on its machine. schmeide does not handle
  agent API keys. A Claude Code session's environment keeps `ANTHROPIC_*` and
  `CLAUDE_*` so that login still works.

## Job lifecycle

| Step | What happens | When |
|---|---|---|
| 1 | An issue is assigned and marked ready with a label. | Done |
| 2 | GitHub delivers the webhook. The control node creates one job, with the assignee as steward, and stores the issue title and body from that delivery. | Done |
| 3 | A runner belonging to the steward pulls the job. | Done |
| 4 | If the steward has no runner available, a pool runner takes the job. | Later |
| 5 | The runner creates a git worktree on a new branch and starts a fresh harness session with a brief built from the title and body stored on the job. | Done |
| 6 | The runner runs the repo's gate command. A gate failure is commented on the issue, without the command output, and no PR is opened. | Done |
| 7 | The runner pushes the branch and opens a PR as the schmeide GitHub App. | Done |
| 8 | An AI reviewer checks the PR. The worker fixes what the reviewer finds until the review passes or the round limit is reached. | Done |
| 9 | With the review passed and CI green, the PR is merged by the factory or by a person, depending on the repo's config. | Later |

Steps 1 to 3 and 5 to 7 shipped in the first slice (#3, #6, and the follow-ups
under [First slice](#first-slice)). Step 8, the AI review stage, has shipped.
Steps 4 and 9 have not been built.

## Decisions

### Intake

GitHub issues are the unit of work and the team-visible queue. An issue becomes
a job when it is open, assigned, and carries the ready label, and the webhook
action is `labeled` or `assigned`. Edits and reopens do not create a job and do
not change one that already exists.

### Routing

A job goes to its steward's own runners. Several runners for one steward share
one queue consumer, so each of that steward's jobs goes to one of them. There
is no fallback to anyone else's runner.

Pool fallback, if it is built, would send a job to a runner that has opted in
to taking other people's work when the steward's own runners cannot take it.
Pool runners would spend their owner's agent subscription on someone else's
job, so the opt-in rules and limits need to be settled before that fallback is
turned on. See [Open decisions](#open-decisions).

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
requests changes and a round remains, a fresh worker session applies the
findings, the gate runs again, and the same branch is pushed. A pass finishes
the job and does not merge. Exhausting the rounds, or a gate failure on a
fix, comments on the issue and leaves the pull request open.

### Merge

Each repo chooses in its tracked config whether the factory merges a PR that
has passed review and CI, or whether a person presses merge. Merging is not
built, and the repo config schema has no merge setting yet.

### PR shape

PRs are independent by default: one issue, one PR against the default branch.
The branch name is `<issue number>_<short-description>`, taken from the issue
title stored on the job. A job whose repository already has a pull request, open
or closed, on a branch starting with `<issue number>_` is treated as already
opened, including after the issue is retitled. The check looks at the 100 pull
requests returned by one list call (`state=all&per_page=100`). PRs are stacked
only when an issue declares a dependency on another issue. Stacked PRs are not
built.

### Harnesses

Harnesses are pluggable. The runner drives a harness through an adapter
interface that states what every harness must support. Claude Code is the
first and only implementation (`internal/harness`). A second harness is later.

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
reviewed like code. The file is `.schmeide/config.toml` and the only key is
`gate`, the command that must pass before a PR is opened. Unknown keys are an
error. The runner reads the file before the harness session, so the session
cannot change the gate it is about to be held to. Other settings (merge
policy, reviewer prompts) are added to the schema when the feature that reads
them is built. Where a pull request dependency is declared is open. See
[Open decisions](#open-decisions).

### Credentials and the job environment

A runner stores no long-lived GitHub credential. When it takes a job it asks
the control node on `schmeide.token.<steward>`, with the job's subject as the
request. The control node loads that job from the stream and mints a GitHub
App installation token limited to the job's repository and to `contents:
write`, `pull_requests: write`, and `issues: write`
(`internal/control/token.go`). The request is refused when the subject is not
one of that steward's jobs. GitHub expires an installation token after one
hour. Git receives the token as an `http.extraHeader` in that git command's
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
`SSL_CERT_FILE`, `SSL_CERT_DIR`, any name starting with `LC_` or `XDG_`, and
any name passed in `--pass-env`. A harness session also sees names starting
with `ANTHROPIC_` or `CLAUDE_`. The runner starts the session with that
environment, so the session does not see the runner's `SCHMEIDE_*` settings,
including its NATS token.

The worktree runs git and the gate with whatever environment it was given, and
adds the installation token only to git's environment. The runner does not pass
the base list when it creates the worktree (`internal/runner/runner.go`). Git's
environment is then only the token variables, and the gate inherits the runner
process's environment. The gate's output is not posted to the issue. The
comment names the command and the failure, and the output tail stays in the
runner's log.

### Deployment

schmeide is deployment-agnostic. The control node is a plain binary configured
by flags, environment variables, or a file, and nothing in the code, config
schema, or defaults may assume a particular host.

There are two requirements wherever it runs:

- GitHub must be able to reach the control node's webhook endpoint, `POST
  /webhook` on `--http-addr`. The listener is HTTP.
- Runners must be able to reach the control node's NATS address.

For quark, the control node will be hosted through the `iac` repo. That is a
suggested deployment pattern and belongs in deployment docs, not in the design
of the tool. A control node without a public address needs a tunnel for
webhooks.

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
  to `--max-attempts` (default 3). The runner counts those work failures
  itself, and a token hand-back or a shutdown does not consume an attempt. The
  wait before the next try is 30 seconds and has no flag. A gate failure is
  final. A pull request on a branch named `<issue number>_*` suppresses a
  second PR.
- **#16.** Webhooks are limited to an allowlist of orgs or App installations.
- **Pull request #30.** Runner credentials are scoped per steward, the job
  carries the issue text from the event that created it, the harness session
  runs with the reduced environment above, and the allowlist is required. An
  empty allowlist allows no job. A job queued before that change has no issue
  text; the runner fails it and says to remove the ready label and add it
  again.

The schmeide GitHub App is a prerequisite for a live delivery. Tests use saved
webhook payloads, a fake GitHub API, and a fake harness.

The AI review stage shipped after this slice. See [Review](#review).

Not in the first slice, and not shipped:

- Falling back to a pool of other people's runners
- Merging
- Stacked PRs for declared issue dependencies
- Container isolation for jobs
- Harnesses other than Claude Code
- Polling GitHub for issues
- More than one job at a time on a runner

## Settled decisions

| Decision | Outcome | Where |
|---|---|---|
| Name of the ready label | `schmeide:ready`. Override with `--ready-label`. | `internal/control/intake.go` (`DefaultReadyLabel`) |
| Path and format of the per-repo config file | `.schmeide/config.toml`. The only key is `gate`, and it must be a non-empty command. Unknown keys are an error. | `internal/repoconfig` |
| Which orgs or App installations may create jobs | Required. `--allowed-orgs` (owner logins, compared case-insensitively) or `--allowed-installations` (numeric installation IDs), or both. A job is accepted when it matches either list. `schmeide control` refuses to start when both are empty, and an empty allowlist allows no job. A delivery that would create a job and matches neither list is rejected with 403. A delivery that does not describe a ready job is ignored, including one from outside the allowlist. A bad signature is rejected before that check. | `internal/control/intake.go`, `internal/control/cli.go` |
| Shape of the job message | JSON with `repo`, `repo_id`, `issue`, `steward`, `installation_id`, `title`, and `body`, on `schmeide.jobs.<steward>.<repo id>.<issue>`. Created only for an open issue that has an assignee and the ready label, and only from an `issues` action of `labeled` or `assigned`. The steward is `issue.assignee`, or the first of `issue.assignees` when that is empty. One job per repository id and issue number; a repeat delivery is a duplicate and does not replace the stored text. The runner does not fetch the issue. | `internal/control/intake.go` |
| How the runner is verified | Automated tests. The runner tests stand up the embedded queue, a fake GitHub REST API, and a fake harness. Intake tests use saved webhook payloads. | `internal/runner/runner_test.go`, `internal/harness/fake.go`, `internal/control/testdata` |
| What a runner may authenticate as | A per-steward NATS token from `--runner-tokens`. A per-job GitHub App installation token scoped to that repository and to contents, pull requests, and issues write. No long-lived GitHub credential on the runner. | `internal/control/queue.go`, `internal/control/token.go` |
| What a harness session may see | The base list in `internal/runner/env.go`, plus `ANTHROPIC_*` and `CLAUDE_*`, plus `--pass-env`. Not the runner's `SCHMEIDE_*` settings. | `internal/runner/env.go`, `internal/runner/runner.go` |

## Open decisions

None of these are settled. Where a value is given, it is a proposal.

| Decision | Proposal | Affects |
|---|---|---|
| How a runner opts in to pool work, and what limits apply | None yet. Needed before pool fallback is turned on. | Later |
| The merge default (#32) | When a repo omits the merge setting, a person presses merge. The factory merges only when the config says so. | Merging (#23) |
| The dependency declaration (#34) | A pull request dependency is GitHub's issue dependency, not a config key. Stack on the dependency's open pull request, or on the default branch once that pull request has merged. | Stacked pull requests (#25) |
| The second harness (#33) | The second harness shells out to the Grok CLI, the same shape as the Claude Code adapter, with no new module dependency. | #27 |
| Concurrent jobs (#44) | A runner keeps working one job at a time. | None of the later items |

## Later

Roughly in the order they are expected to matter:

1. Merging, with the per-repo choice between factory merge and human merge
2. Pool fallback, with opt-in and limits
3. Stacked PRs for issues that declare a dependency
4. Container isolation for jobs
5. A second harness
