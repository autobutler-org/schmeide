# Review journeys

Covers the reviewer rounds that run after the pull request opens, and the fix
rounds a writer gets for review findings. Implemented in
`internal/runner/review.go`. Configured by the runner's `--max-review-rounds`
(default 3) and the repo's `[automate] review_fix` ([../config.md](../config.md)).

While the review runs, `schmeide status` shows the job as `reviewing` with
the detail `round <n> of <max>`.

---

### JN-REV-001: A review starts when the pull request opens

**Preconditions:** JN-PR-012: a pull request just opened.

**Steps:**

1. Watch the runner's log, or run `schmeide status`.

**Expected result:**

- The runner logs `msg="job state" ... state=reviewing detail="round 1 of 3"`.
- A fresh reviewer session runs in the same worktree. It is told not to edit,
  commit, or push, and to write its verdict to `.schmeide/review.md`.

**Notes:**

- The reviewer is the first ready harness that is not the writer. With Claude
  Code and Grok both ready, Grok reviews Claude Code's change. With one
  harness, it reviews its own change.
- The reviewer's brief has the issue's title and body from delivery time
  (JN-PR-011).
- Covered by `TestReviewRunsOnTheOtherReadyHarness` and
  `TestReviewStaysOnWriterWhenNoOtherHarnessIsReady`.

---

### JN-REV-002: The review passes

**Preconditions:** JN-REV-001. `[automate] merge` is off.

**Steps:**

1. The reviewer writes `pass` as the only line of `.schmeide/review.md`.

**Expected result:**

- The issue gets: "schmeide opened a pull request for this issue: <url>" and,
  after a blank line, "The review passed."
- The runner logs `state=pr_opened detail=<url>`. The job leaves
  `schmeide status`.
- The pull request stays open for a person (JN-MERGE-001).

**Notes:**

- With merge on, a pass goes on to the factory merge instead and the issue
  gets the merge outcome comment (JN-MERGE-002).
- Covered by `TestReviewPassesOnFirstRound` and
  `TestJourneyLabeledIssueOpensOnePullRequest`.

---

### JN-REV-003: The verdict file is never committed

**Preconditions:** Any review round.

**Steps:**

1. After the job, open the pull request's files.

**Expected result:**

- `.schmeide/review.md` is not in the pull request. The runner reads it and
  removes it before anything else is committed.

**Notes:**

- A fix round also removes `.schmeide/pr.md` and `.schmeide/review.md` before
  its commit.
- Covered by `TestReviewChangesThenPassUpdatesSameBranch` and `TestReadVerdict`.

---

### JN-REV-004: Changes requested with `review_fix` off

**Preconditions:** The repo has no `[automate] review_fix`, or it is `"off"`.

**Steps:**

1. The reviewer writes `changes` on the first line and its findings below.

**Expected result:**

- No fix round runs. The pull request is left open.
- The issue gets: "schmeide left the pull request open for a person because
  the review requested changes and automate.review_fix is off: <url>".
- The job ends `review_failed`. It is not retried, and with merge on it is
  not merged.

**Notes:**

- The findings are not posted. A person reads the pull request.
- Covered by `TestReviewFixOffLeavesARejectedPullRequestOpen` and
  `TestReviewFixOffWithMergeOnDoesNotMergeARejection`.

---

### JN-REV-005: Changes requested with `review_fix` on

**Preconditions:** `[automate] review_fix = "on"`. `--max-review-rounds` is 3.

**Steps:**

1. The round 1 reviewer requests changes.
2. Watch the pull request.

**Expected result:**

- The runner logs `state=working detail=claude-code`. A fresh worker session
  gets the findings and applies them.
- A commit "Apply review feedback for #<n>" is added. The gate runs again.
- The same branch is pushed (no force), and the open pull request shows the
  new commit.
- The runner logs `state=reviewing detail="round 2 of 3"`, and a fresh
  reviewer runs. A pass ends as JN-REV-002.

**Notes:**

- Each fix round is a fresh session. It does not see earlier sessions.
- Covered by `TestReviewChangesThenPassUpdatesSameBranch`.

---

### JN-REV-006: The review does not pass within the rounds

**Preconditions:** `review_fix = "on"`, `--max-review-rounds 3`. Every
reviewer requests changes.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- Rounds 1 and 2 each get a fix round. Round 3's rejection is the result: no
  third fix runs.
- The issue gets: "schmeide failed this job because the review did not pass
  within 3 rounds and left the pull request open: <url>".
- The job ends `review_failed`, is not retried, and is not merged.

**Notes:**

- With `--max-review-rounds N`, a job gets at most N-1 review fix rounds.
- Covered by `TestReviewExhaustingRoundsFailsAndDoesNotMerge`.

---

### JN-REV-007: The gate fails on a fix round

**Preconditions:** `review_fix = "on"`. A reviewer requests changes, and the
fix breaks the gate.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The fix commit is not pushed. The pull request keeps its last pushed head.
- The issue gets: "schmeide left the pull request open: the gate command
  `<gate>` failed: exit status 1. The gate's output is in the runner's log.
  <url>".
- The job ends `gate_failed`. No further review round runs, and it is not
  retried.

**Notes:**

- A gate timeout on a fix round gives "schmeide left the pull request open:
  the gate timed out. <url>".
- Covered by `TestReviewFixGateFailureStopsTheReview`.

---

### JN-REV-008: A fix round makes no changes

**Preconditions:** `review_fix = "on"`. The fix session changes nothing.

**Steps:**

1. Assign and label an issue whose review requests changes.

**Expected result:**

- The issue gets "schmeide failed this job and left the pull request open:
  <url>" and, after a blank line, "the claude-code session made no changes for
  the review findings". The job ends `review_failed`.

**Notes:**

- A fix session that fails or times out ends the same way, with "the
  claude-code session did not finish: ..." or "the claude-code session timed
  out".
- No automated test for the no-change case.

---

### JN-REV-009: The reviewer fails or writes a bad verdict

**Preconditions:** Any review round.

**Steps:**

1. The reviewer session exits non-zero, times out, or writes a verdict the
   runner cannot read.

**Expected result:**

- The issue gets "schmeide failed this job and left the pull request open:
  <url>" followed by one of:
  - "the grok reviewer session did not finish: ..." (session failed)
  - "the grok reviewer session timed out" (session timeout, JN-FAIL-004)
  - "the grok reviewer session produced no output for 15m0s" (session idle
    limit, JN-FAIL-023)
  - "the reviewer did not write .schmeide/review.md with a first line of
    "pass" or "changes": ..." (no file)
  - "the reviewer wrote "maybe" as its verdict; the first line of
    .schmeide/review.md must be "pass" or "changes"" (unknown first line)
  - "the reviewer wrote a pass verdict with extra text in .schmeide/review.md;
    a pass is the single line "pass"" (pass with more text)
  - "the reviewer requested changes but did not say what to fix" (`changes`
    alone)
- The job ends `review_failed` and is not retried.

**Notes:**

- The harness name in the message is the reviewer's (`claude-code` or `grok`).
- Covered by `TestReadVerdict` and `TestReviewerSessionFailureIsFinal`.

---

### JN-REV-010: Change the number of review rounds

**Preconditions:** A runner.

**Steps:**

1. Start the runner with `--max-review-rounds 1` (or
   `SCHMEIDE_MAX_REVIEW_ROUNDS=1`).
2. Assign and label an issue in a repo with `review_fix = "on"` whose review
   requests changes.

**Expected result:**

- One reviewer round runs. Its rejection ends the job with "...did not pass
  within 1 rounds..." and no fix round runs.
- With merge on, a CI fix round is also not available (JN-MERGE-008).

**Notes:**

- Zero or a negative value means the default, 3.
- The flag is the runner's, so it applies to every repository that runner
  serves.
- Covered by `TestRunnerHelpDocumentsReviewRounds`.

---

### JN-REV-011: Spent rounds survive a redelivery

**Preconditions:** `review_fix = "on"`. One fix round was pushed, then the
runner died during round 2 (JN-FAIL-010).

**Steps:**

1. Start a runner again and wait for the redelivery.

**Expected result:**

- The job resumes at review without opening a second pull request. It counts
  the "Apply review feedback for #<n>" commits on the branch as rounds already
  spent, and starts at round 2 rather than round 1.

**Notes:**

- A round that started but whose fix never reached the branch is run again.
  A rejection whose fix was never pushed is not counted, so a redelivery can
  run one round more than `--max-review-rounds`.
- Covered by `TestRedeliveryKeepsSpentReviewRounds` and
  `TestResumeAtReviewFromTheStageRecord`.
