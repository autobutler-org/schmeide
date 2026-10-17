# Failure journeys

Covers what the runner does when a job goes wrong: retries, timeouts, the
secret scan, delivery heartbeats, redelivery after a dead runner, stuck jobs,
and the branch guards. Implemented in `internal/runner` (`runner.go`,
`workspace.go`), `internal/control/queue.go`, `internal/secretscan`, and
`internal/refguard`. See [../FAILURE-MODES.md](../FAILURE-MODES.md).

Where to look:

- **The issue** gets one outcome comment when the job ends. A retried attempt
  posts nothing.
- **The runner's stderr** has every state change (`msg="job state" repo=...
  issue=... state=... detail=...`), errors that never reach the issue (token
  hand-backs, stuck jobs), the gate's output tail, and the harness session's
  own output.
- **`schmeide status`** shows unfinished jobs with their latest state and
  detail (JN-CLI-007).
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
  no changes; a config error (JN-CFG-002); a trust refusal (JN-SETUP-022); a
  secret scan refusal (JN-FAIL-007); a git or GitHub error before the pull
  request is open; a non-linear stack of blockers (JN-PR-019).
- Not retried, final at once: a gate failure or gate timeout, any review
  failure, any factory merge failure.
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
`config.go`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- `git push` does not run. The branch never reaches GitHub.
- The attempt fails with "refusing to push: config.go:12 contains a GitHub
  token" and is retried. After the last attempt the issue comment carries
  that text. The secret itself is never in the message.

**Notes:**

- The scan reads tracked files in the worktree and looks for PEM private
  keys, GitHub tokens, GitLab tokens, AWS access keys, and JWTs. It does not
  read the environment.
- Several findings are joined with "; ".
- The same scan runs before a restack push (JN-MERGE-018).
- Covered by `TestPushStopsForPrivateKeyWithoutRevealingIt` and
  `TestScanReportsEachCredentialShape`.

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
  later in the job is JN-FAIL-020.
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

### JN-FAIL-020: A job outlives its first token

**Preconditions:** A job is still working more than 50 minutes after its
claim: a long harness session, several review rounds, or a long
`check_wait`.

**Steps:**

1. Assign and label an issue whose job runs that long.

**Expected result:**

- Before each GitHub call and each git command, the runner checks the job's
  token. When it has less than 10 minutes left before its `expires_at`, the
  runner asks the control node on `schmeide.token.<steward>` again and uses
  the new token. Push, opening the pull request, review comments, check
  polling, and the merge do not fail with 401.
- If that request fails while the held token has not expired, the runner
  keeps using the held token and asks again on the next call.
- If it fails after the held token expired, the GitHub call fails with "get
  the job's GitHub token: the control node could not mint a token: ...", or a
  git command fails with "get the job's GitHub token for git <args>: ...".
  That error is handled like any other GitHub or git error at that point in
  the job: before the pull request is open it is a failed attempt; after, the
  outcome comment says it and the pull request stays open.

**Notes:**

- The control node mints a new token for every request it accepts. It does
  not limit how many tokens one job asks for.
- Covered by `TestJobTokenRefresh` and `TestJobRefreshesAnExpiringToken` in
  `internal/runner`.
