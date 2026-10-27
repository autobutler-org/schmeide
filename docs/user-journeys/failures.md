# Failure journeys

Covers what the runner does when a job goes wrong: retries, timeouts, the
secret scan, delivery heartbeats, redelivery after a dead runner, stuck jobs,
the branch guards, a gate that fails because of the runner's environment, and
sessions the permission floor keeps from running commands. Implemented in `internal/runner` (`runner.go`,
`workspace.go`), `internal/control/queue.go`, `internal/secretscan`, and
`internal/refguard`. See [../FAILURE-MODES.md](../FAILURE-MODES.md).

Where to look:

- **The issue** gets one outcome comment when the job ends. A retried attempt
  posts nothing.
- **The runner's stderr** has every state change (`msg="job state" repo=...
  issue=... state=... detail=...`), errors that never reach the issue (token
  hand-backs, stuck jobs), the gate's output tail, and the harness session's
  own output.
- **`schmeide status`** shows unfinished jobs with their latest state, how long
  they have been in it, and detail (JN-CLI-007). A job no runner has claimed
  yet is `pending` (JN-CLI-016).
- **The control node's stderr** has webhook outcomes, registrations, and every
  job state the runners report.

---

### JN-FAIL-001: A failed attempt is retried

**Preconditions:** A runner with the default `--max-attempts 3`. The harness
fails once (for example an API outage), then works.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The runner logs `state=retrying detail="attempt 1 of 3 failed: the
  claude-code session did not finish: ..."`. `schmeide status` shows the same.
- No comment on the issue.
- 30 seconds later the job is delivered again, starts over, and opens a pull
  request.

**Notes:**

- Failures that count as an attempt: a session that fails, times out, or makes
  no changes; a trust refusal (JN-SETUP-022); a secret scan refusal
  (JN-FAIL-007); a git or GitHub error before the pull request is open.
- Not retried, final at once: a missing or invalid repo config (JN-CFG-002),
  open blockers side by side (JN-PR-019), a gate failure or gate timeout, any
  review failure, any factory merge failure.
- Not counted at all: a token hand-back (JN-FAIL-015), a shutdown hand-back
  (JN-FAIL-016), a stuck-job hand-back (JN-FAIL-014).
- The 30-second wait has no flag.
- Covered by `TestFailedJobIsRetried`.

---

### JN-FAIL-002: A failure becomes final after `--max-attempts`

**Preconditions:** The harness fails every time.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- Attempts 1 and 2 report `retrying`. Attempt 3 is final.
- The issue gets: "schmeide could not finish this job and did not open a pull
  request:" and, after a blank line, the last error.
- The job ends `failed`. Relabeling the issue creates a new job (JN-PR-005).

**Notes:**

- `--max-attempts 1` makes every failure final.
- Covered by `TestFailedJobIsFinalAfterMaxAttempts`.

---

### JN-FAIL-003: The attempt count survives a runner restart

**Preconditions:** A job has used one attempt (status `retrying`).

**Steps:**

1. Stop the runner (Ctrl-C) and start it again.

**Expected result:**

- The redelivered job continues from attempt 2, not 1. A restart does not
  grant a fresh budget.

**Notes:**

- The count is stored with each job status in the control node's data
  directory.
- Covered by `TestAttemptCountSurvivesRunnerRestart`.

---

### JN-FAIL-004: A session times out

**Preconditions:** A runner with `--session-timeout 30m`, or a repo with
`session_timeout = "30m"`. The writer session runs longer.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- At 30 minutes the session's whole process group is killed.
- The attempt fails with "the claude-code session timed out" and is retried
  (JN-FAIL-001). After the last attempt the issue gets "schmeide could not
  finish this job and did not open a pull request:" with "the claude-code
  session timed out". No session output is in the comment.

**Notes:**

- A timeout in a reviewer or fix session after the pull request is open is
  final (JN-REV-008, JN-REV-009).
- Covered by `TestSessionTimeoutFailsTheJob`.

---

### JN-FAIL-005: The gate times out

**Preconditions:** `--gate-timeout` or `gate_timeout` is shorter than the
gate takes.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- At the deadline the gate's process group is killed.
- The issue gets "schmeide did not open a pull request: the gate timed out.".
  No output. The job ends `gate_failed` and is not retried.
- The runner logs `level=WARN msg="gate timed out"`.

**Notes:**

- Covered by `TestGateTimeoutFailsTheJob`.

---

### JN-FAIL-006: A gate failure is not retried

**Preconditions:** A failing gate.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- One attempt only. See JN-PR-014 for the comment.

**Notes:**

- Covered by `TestGateFailureOpensNoPR`.

---

### JN-FAIL-007: The secret scan blocks a push

**Preconditions:** The session commits a file with a credential-shaped
literal, for example a line with `ghp_` followed by 36 characters in
`config.go`, or the job's installation token or the runner's NATS token
verbatim.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- `git push` does not run. The branch never reaches GitHub.
- The attempt fails with "refusing to push: config.go:12 contains a GitHub
  token" and is retried. After the last attempt the issue comment carries
  that text. The secret itself is never in the message.

**Notes:**

- The scan reads only the lines the branch adds relative to its base
  (`git diff -U0 -M <base>...HEAD`) and looks for PEM private keys, GitHub
  tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), GitLab tokens,
  AWS access keys, JWTs, Anthropic API keys (`sk-ant-`), and xAI API keys
  (`xai-`). It does not read the environment.
- The job's installation token and the runner's NATS token are refused
  verbatim, as "contains a runner credential", even on a line marked
  `schmeide:allow-secret`.
- A literal already on the base, or carried over by a rename, does not block
  the push.
- Several findings are joined with "; ".
- The same scan runs before a restack push (JN-MERGE-018).
- Covered by `TestPushStopsForPrivateKeyWithoutRevealingIt`,
  `TestPushRefusesTheJobsCredentialsAndNewTokenShapes`, and
  `TestPushIgnoresALiteralAlreadyOnTheBase` in `internal/runner`, and
  `TestScanReportsEachCredentialShape`, `TestScanDiffOnAddedLines`, and
  `TestScanDiffReadsOnlyTheBranchsAddedLines` in `internal/secretscan`.

---

### JN-FAIL-008: Allow a known test fixture through the secret scan

**Preconditions:** A tracked file has a fake key on purpose.

**Steps:**

1. Add `schmeide:allow-secret` on the same line as the literal, for example in
   a trailing comment.
2. Assign and label an issue.

**Expected result:**

- That line is skipped, and the push goes ahead.

**Notes:**

- Covered by `TestPushAllowsALineMarkedAllowSecret` and
  `TestScanSkipsAllowCommentOnTheSameLine`.

---

### JN-FAIL-009: A long job keeps its delivery with heartbeats

**Preconditions:** A runner with the default `--ack-wait 5m`. A session that
runs for an hour.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The job is not redelivered while the runner is alive and connected. The
  runner tells JetStream the job is in progress every third of `--ack-wait`.

**Notes:**

- The runner must stay connected to the control node for the whole job. If
  heartbeats stop for `--ack-wait`, the job is delivered again (JN-FAIL-010).
- Covered by `TestDeliveryLeaseFlagDefaults`. No automated test of a
  long-running heartbeat.

---

### JN-FAIL-010: A runner killed after the pull request opens

**Preconditions:** A job's pull request is open and the review is running.

**Steps:**

1. Kill the runner process (`kill -9`).
2. Start a runner for the same steward.

**Expected result:**

- After `--ack-wait`, the new runner logs `state=claimed` for the job.
- It finds the open pull request (by the `<issue>_` branch prefix, or by the
  stage record when GitHub's list lags), resumes at review on that branch,
  and opens no second pull request.
- The review finishes and the issue gets the outcome comment.

**Notes:**

- Kill any orphaned harness process too: sessions run in their own process
  group.
- Covered by `TestJourneyKilledRunnerJobIsRedeliveredWithoutASecondPullRequest`
  (`internal/e2e`), `TestDeadRunnersJobIsRedeliveredWithoutDuplicatePR`,
  `TestResumeAtReviewFromTheStageRecord`.

---

### JN-FAIL-011: A runner killed before the pull request opens

**Preconditions:** A job's writer session is running. No pull request yet.

**Steps:**

1. Kill the runner and start it again.

**Expected result:**

- After `--ack-wait` the job is delivered again and starts over: a fresh
  worktree from the base branch, a fresh session. Leftovers from the first
  attempt are removed.

**Notes:**

- A kill does not count as an attempt, because no failure was reported.
- No automated test for the kill-before-PR case.

---

### JN-FAIL-012: The outcome was posted but not acknowledged

**Preconditions:** A runner posted the outcome comment on the issue and died
before it acknowledged the job.

**Steps:**

1. Start a runner and wait for the redelivery.

**Expected result:**

- The runner finds its own outcome comment posted since the pull request
  opened, and finishes the job with that state. No second comment, no new
  review.

**Notes:**

- The runner reads the hidden `<!-- schmeide:outcome <state> -->` marker in
  comments from bots.
- Covered by `TestRedeliveryAfterOutcomeCommentPostsNothing`.

---

### JN-FAIL-013: A person closed the pull request before a redelivery

**Preconditions:** A job was redelivered after a runner died, and a person
had closed its pull request.

**Steps:**

1. Start a runner and wait for the redelivery.

**Expected result:**

- The runner treats the closed pull request as a decision. It reports the job
  `pr_opened`, posts nothing, and opens no new pull request.

**Notes:**

- No automated test.

---

### JN-FAIL-014: `--stuck-after` hands back a job with no progress

**Preconditions:** A runner with `--stuck-after 1h`. A job reports no state
change for an hour (for example a session that hangs under a longer
`--session-timeout`).

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The runner logs `level=ERROR msg="job is stuck; handing it back"
  ... stuck_after=1h0m0s`, cancels the work, and hands the job back at once.
- No comment. The attempt count does not change. Another runner of the
  steward, or this one, gets the job again.

**Notes:**

- Set it longer than the longest legitimate phase, including
  `--session-timeout`, `--gate-timeout`, and a factory merge's `check_wait`.
- Covered by `TestStuckJobRecovery`.

---

### JN-FAIL-015: No token for the job

**Preconditions:** The control node cannot mint an installation token, for
example because the App was uninstalled from the repository, or because the
App or its installation lacks one of the permissions the token asks for
(Contents, Pull requests, Issues, Checks, Administration, Actions).

**Steps:**

1. Have a job delivered to a runner.

**Expected result:**

- The runner logs `level=ERROR msg="handing the job back"` with "the control
  node could not mint a token: github refused an installation token for
  installation <id>: ...".
- The control node logs `msg="token request failed"`.
- No comment (the issue cannot be reached without a token). The job is offered
  again 30 seconds later, without using an attempt.

**Notes:**

- This repeats until the token works or the job is removed.
- A missing permission shows in the GitHub reply quoted after "github refused
  an installation token for installation <id>:". Grant the permission to the
  App and accept it on the installation.
- Only the first token request of a delivery is handled this way. A refresh
  later in the job is JN-FAIL-021.
- Covered by `TestTokenHandBackDoesNotConsumeAttempts` and
  `TestMintReportsGitHubRefusal`.

---

### JN-FAIL-016: Stopping a runner mid-job

**Preconditions:** A runner working a job.

**Steps:**

1. Press Ctrl-C (SIGINT) or send SIGTERM.

**Expected result:**

- The session and gate are canceled, the worktree is removed, and the job is
  handed back at once for another runner. No comment. No attempt used.
- The runner exits 0.

**Notes:**

- Covered by `TestShutdownHandBackDoesNotConsumeAttempts`.

---

### JN-FAIL-017: A session cannot move another job's branch

**Preconditions:** A job for issue 12 is running. The session tries to create,
move, or delete `refs/heads/5_other` in its worktree.

**Steps:**

1. Let the session run `git branch 5_other` or `git branch -D 5_other`.

**Expected result:**

- Git refuses, and prints "schmeide: this job worktree may move only
  refs/heads/12_<slug>, not refs/heads/5_other".
- Branches whose names are not `<digits>_*` are not covered.

**Notes:**

- The guard is a `reference-transaction` hook installed in the worktree. It
  is git-ignored and never committed.
- Covered by `TestJobWorktreeMovesOnlyItsOwnBranch` and
  `TestNewWorktreeGuardsOtherJobBranches`.

---

### JN-FAIL-018: The job's branch moved on GitHub

**Preconditions:** Someone pushed a commit to the job's branch on GitHub that
the runner's checkout does not contain.

**Steps:**

1. Let the runner reach its push.

**Expected result:**

- The push does not overwrite the branch. The attempt fails with "the branch
  <branch> moved on the remote and was not overwritten; someone has to look
  at that branch", and the foreign commit is kept.

**Notes:**

- Covered by `TestForeignJobBranchCommitIsKept`.

---

### JN-FAIL-019: Find out why a job failed

**Preconditions:** A job ended with a failure comment, or is stuck in
`retrying`.

**Steps:**

1. Read the issue's outcome comment.
2. Run `schmeide status --steward <login> --nats-token <token>` for a job that
   is still running or retrying.
3. Search the runner's stderr for `issue=<n>`.

**Expected result:**

- The comment says what failed, without command output.
- The status line shows the latest state and detail, such as `retrying
  attempt 2 of 3 failed: ...`.
- The runner log has each state change, the gate output tail (`msg="gate
  failed" ... output=...`), and the harness session's output.

**Notes:**

- No automated test.

---

### JN-FAIL-020: A runner started in an interactive terminal

**Preconditions:** An operator runs `schmeide runner` in the foreground of an
interactive terminal, so the runner's stderr, and with it every harness
session's output, is that terminal.

**Steps:**

1. Assign and label an issue.
2. While the session runs, run `ps -o pid,pgid,sid,tpgid,stat,tty,args` for
   the runner and the `claude` or `grok` process.

**Expected result:**

- The harness session's session id equals its own pid, and it is not stopped
  (no `T` in `stat`). It writes its output to the terminal and finishes in the
  time it takes without a terminal.
- The gate command also leads its own session.
- The job does not end with "the claude-code session timed out" because the
  session was stopped by `SIGTTOU`.

**Notes:**

- Each session and gate starts with `Setsid` (`internal/harness/claude.go`,
  `internal/harness/grok.go`, `internal/runner/workspace.go`). See
  [../FAILURE-MODES.md](../FAILURE-MODES.md#a-runner-started-in-a-terminal).
- Covered by `internal/harness` `TestFakeagentRunsInItsOwnSession` and
  `internal/runner` `TestGateRunsInItsOwnSession`. No test runs the runner
  under a real terminal.

---

### JN-FAIL-021: A job that outlives its token

**Preconditions:** A job whose work runs longer than its first installation
token lives. GitHub expires a token an hour after it is minted, and the
default `session_timeout` is two hours, so a long session, many review rounds,
or a long `check_wait` is enough.

**Steps:**

1. Have a job delivered to a runner.
2. Let the session (or the review and CI wait after it) run past the first
   token's `expires_at`.

**Expected result:**

- Before each GitHub call and each git command, the runner checks the token it
  holds. Once that token is within five minutes of its `expires_at`, the runner
  asks the control node on `schmeide.token.<steward>` for a new one for the same
  job, and the control node mints one.
- The push, the pull request, the review rounds, the merge, and the outcome
  comment all use an unexpired token. GitHub never answers `401 Bad
  credentials`, and the steward gets the usual outcome comment, including the
  failure comment of a job whose session timed out (JN-FAIL-004).
- When the control node cannot mint the new token, the call that needed it
  fails with "refresh the job's GitHub token: ..." and the job fails as for any
  GitHub error. The outcome comment is still tried with its own refresh. If
  that fails too, the runner logs `level=ERROR msg="could not comment on the
  issue"` with the refresh error, and `schmeide status` and the runner log are
  where the outcome shows.

**Notes:**

- A token without an `expires_at` is never refreshed.
- Each refresh mints another token, so a long job mints several.
- Covered by `TestJourneyJobOutlivingItsTokenStillOpensItsPullRequest`
  (`internal/e2e`), and in `internal/runner` by `TestJobOutlivingItsTokenOpensPR`,
  `TestSessionTimeoutAfterTokenExpiryStillComments`,
  `TestWorktreeGitUsesTheCurrentToken`, `TestJobTokenRefreshesOnlyWhenStale`,
  and `TestTokenStale`; in `internal/forge` by `TestTokensIsAskedForEachCall`.

---

### JN-FAIL-022: The job's installation token is revoked when the delivery ends

**Preconditions:** A runner is connected and the control node can mint
installation tokens.

**Steps:**

1. Assign and label an issue.
2. Let the job reach any end: a pull request, a gate failure, a failed attempt
   that is retried, or a final failure.

**Expected result:**

- After the outcome comment and the acknowledgment, the runner calls
  `DELETE /installation/token` with the token the delivery holds at that
  point. The token stops working at once instead of at the end of its hour.
  A token a refresh replaced (JN-FAIL-021) is not revoked; it was within five
  minutes of its expiry.
- Each delivery mints its own token, so a job retried three times revokes three
  tokens.
- If GitHub refuses the revoke, the runner log has "could not revoke the job's
  token; it expires within the hour" with the repo and issue. The job's state,
  its issue comment, and its acknowledgment are unchanged.

**Notes:**

- Implemented by `github.RevokeToken` (`internal/runner/github.go`) and
  `Runner.revoke` (`internal/runner/runner.go`).
- Covered by `TestJobEndRevokesItsToken`,
  `TestFailedRevokeIsLoggedAndKeepsTheOutcome`,
  `TestFailedJobIsFinalAfterMaxAttempts`, and
  `TestJobOutlivingItsTokenOpensPR` (the revoke uses the refreshed token) in
  `internal/runner`, and
  `TestRevokeToken` in `internal/testutil/fakegh`.

---

### JN-FAIL-023: A session goes silent

**Preconditions:** A runner with the default `--session-timeout` of 2h. The
writer session stalls: it makes one API request and then writes nothing more.

**Steps:**

1. Assign and label an issue.
2. Watch the runner's stderr. Claude Code runs with `--output-format
   stream-json --verbose`, so each session event is printed as one JSON line
   as it happens, and the last line shows where the session stopped.

**Expected result:**

- 15 minutes after the session's last output, long before the session
  timeout, the session's whole process group is killed.
- The attempt fails with "the claude-code session produced no output for
  15m0s", not "timed out". The job's status goes to `retrying` with the
  detail "attempt 1 of 3 failed: the claude-code session produced no output
  for 15m0s", and the job is retried (JN-FAIL-001).
- After the last attempt the issue gets "schmeide could not finish this job
  and did not open a pull request:" followed by "the claude-code session
  produced no output for 15m0s". No session output is in the comment.
- A session that keeps writing, however slowly, is not killed by the idle
  limit; the session timeout still bounds it (JN-FAIL-004).

**Notes:**

- The limit is 15 minutes because one Bash tool call may run for up to 10
  minutes with no output. It has no flag or config key.
- A reviewer that goes silent fails with "the grok reviewer session produced
  no output for 15m0s", which is final like a reviewer timeout (JN-REV-009).
- Covered by `internal/runner` `TestSessionIdleLimitRetriesThenFails` (silent
  session, retry, comment) and `TestRunHarnessIdle` (silent reviewer, a
  session that keeps writing, the session timeout still applying), and
  `internal/harness` `TestClaudeRun` (the stream-json flags).

---

### JN-FAIL-024: Every job fails the gate because of the runner's environment

**Preconditions:** A repo whose gate needs state that is not in git or not in
the gate's environment: resolved packages such as Dart's `.dart_tool/` and
`~/.pub-cache` or a `node_modules/`, an SDK cache in the home directory, or an
SDK version newer than the one on the runner's `PATH`. The same gate passes in
the operator's own checkout.

**Steps:**

1. Assign and label any issue, even one that asks for a one-line change.
2. Read the runner's `level=WARN msg="gate failed"` line.
3. Clone the repo fresh and run the gate on unmodified `main` with `env -i`,
   the base names, and `HOME` and `TMPDIR` set to an empty directory, as in
   [../FAILURE-MODES.md](../FAILURE-MODES.md#diagnose).
4. Fix the cause: make the gate resolve its own dependencies (for example
   `gate = "flutter pub get && make check test"`), pass the cache location
   with `--gate-pass-env PUB_CACHE`, or upgrade the SDK on the runner. Then
   remove and re-add `schmeide:ready`.

**Expected result:**

- Step 1: the job fails at the gate within minutes, whatever the change. The
  issue gets "schmeide did not open a pull request: the gate command `<gate>`
  failed: exit status 2. The gate's output is in the runner's log." (the exit
  status is the gate's own; `make` returns 2). The job is not retried
  (JN-FAIL-006).
- Step 2: the `output` tail names the missing piece. On a Flutter repo that was
  `Failed to resolve package URI "package:flutter_lints/flutter.yaml"` with
  `Formatted 1894 files (131 changed)`, or `Because quark requires Flutter SDK
  version 3.47.8, version solving failed.` on a machine with 3.47.7.
- Step 3: the gate fails the same way on `main`, which shows the environment,
  not the session's change, is the cause.
- Step 4: the new job's gate passes and the pull request opens (JN-PR-012).

**Notes:**

- The gate runs in a fresh `git worktree add` checkout with `gateEnv` and
  `scratchHome` (`internal/runner/env.go`) and `goCacheEnv`
  (`internal/runner/cache.go`). Only Go's caches outlive the job.
- `HOME`, `TMPDIR`, and the `XDG_*_HOME` names in `--gate-pass-env` have no
  effect: `scratchHome` replaces or drops them.
- The environment is covered by `TestGateEnvKeepsBaseAndGatePassEnvAndDropsSecrets`
  and `TestScratchHomeReplacesHomeTmpdirAndXDGDirs`, and the comment by
  `TestGateFailureOpensNoPR`, in `internal/runner`. No test runs a real
  toolchain.

---

### JN-FAIL-025: The default permission floor stops a session from running commands

**Preconditions:** A runner started with the default `--claude-args`
(`--permission-mode acceptEdits`). A repo whose gate runs a formatter, a
linter, or tests.

**Steps:**

1. Assign and label an issue.
2. Watch the session's output on the runner's stderr.
3. Stop the runner and start it with `--claude-args "--permission-mode auto"`
   and no `--allow-dangerous-harness-args`.
4. Start it again with `--claude-args "--permission-mode auto"
   --allow-dangerous-harness-args` (or `SCHMEIDE_CLAUDE_ARGS="--permission-mode
   auto"` and `SCHMEIDE_ALLOW_DANGEROUS_HARNESS_ARGS=true`), and relabel the
   issue.

**Expected result:**

- Step 2: each Bash tool call comes back "This command requires approval",
  because `--print` mode has no one to approve it. The session edits files
  but runs nothing. A mistake a formatter or test would have caught shows up
  only as a gate failure (JN-PR-014).
- Step 3: the runner exits with code 2 and prints `--claude-args contains
  "--permission-mode auto", which lets a session skip its permission checks;
  remove it, or pass --allow-dangerous-harness-args to accept that (see
  docs/permissions.md)`.
- Step 4: the runner starts, and the session can run the repo's commands with
  every right of the runner's OS user.

**Notes:**

- The floor is deliberate. See [../permissions.md](../permissions.md) and
  [../FAILURE-MODES.md](../FAILURE-MODES.md#sessions-cannot-run-commands-under-the-default-permission-floor).
- "This command requires approval" is Claude Code's text, not schmeide's.
- Covered by `TestClaudeRun` and `TestWideningArg` in `internal/harness`, and
  `TestHarnessArgsPermissionFloor` and `TestMainRefusesWideningHarnessArgs` in
  `internal/runner`. No test runs the real claude CLI.
