# Merge journeys

Covers what happens after the review passes: leaving the pull request for a
person, or the factory merge (`[automate] merge = "on"`). Implemented in
`internal/runner/merge.go`, with the merge queue in `internal/forge/queue.go`
and branch protection in `internal/forge/github.go`. See
[../config.md](../config.md#automatemerge) and
[../GLOSSARY.md](../GLOSSARY.md#factory-merge).

Every factory merge failure is final: the job ends `merge_failed`, is not
retried, and the pull request stays open. Its comment is "schmeide failed this
job and left the pull request open: <url>" followed by a blank line and the
reason quoted in each journey below.

Durations in messages use Go's format, for example `2m0s`.

---

### JN-MERGE-001: Merge off leaves the pull request for a person

**Preconditions:** The repo has no `[automate]` table, or `merge = "off"`.

**Steps:**

1. Assign and label an issue. The review passes (JN-REV-002).

**Expected result:**

- The pull request stays open. The issue gets "schmeide opened a pull request
  for this issue: <url>" and "The review passed.".
- The runner does not read check runs or branch protection.

**Notes:**

- Covered by `TestReviewPassesOnFirstRound`.

---

### JN-MERGE-002: Green checks merge the pull request

**Preconditions:** `merge = "on"`. The repository's CI reports check runs on
pull request heads, and they pass within `check_wait` (default 2m). The base
branch has no merge queue and does not require approving reviews.

**Steps:**

1. Assign and label an issue. The review passes.

**Expected result:**

- The runner reads check runs on the head commit after 1 second, then backs
  off, doubling the pause up to 30 seconds, with a last read at the end of
  the wait.
- Once every check run has completed with `success`, or with `skipped`, the
  pull request is merged with a merge commit (not squash, not rebase). A
  `skipped` conclusion is a path-filtered job, or a matrix job that did not
  run (JN-MERGE-019).
- The issue gets: "schmeide merged the pull request after the review passed
  and CI succeeded: <url>". The job ends `merged`.

**Notes:**

- A check that is still queued or in progress keeps the wait going.
- Covered by `TestFactoryMergeWhenChecksSucceed`,
  `TestMergeWhenGreenMergesWithoutMergeQueue`,
  `TestMergeWhenGreenMergesSlowGreenChecks`.

---

### JN-MERGE-003: A merge queue gets the pull request instead

**Preconditions:** As JN-MERGE-002, and the base branch has a merge queue.

**Steps:**

1. Assign and label an issue. The review passes and checks go green.

**Expected result:**

- The pull request is added to the merge queue, not merged directly.
- The issue gets: "schmeide added the pull request to the merge queue after
  the review passed and CI succeeded: <url>".
- The job ends `queued` and leaves `schmeide status`. The queue merges it.

**Notes:**

- A failure here says "CI is green, but schmeide could not read the merge
  queue: ..." or "CI is green, but schmeide could not add the pull request to
  the merge queue: ...".
- `enqueuePullRequest` needs Merge queues: write. The job token asks for
  `merge_queues: write` (`internal/control/token.go`, pinned by
  `TestTokenPerJob`). An installation that has not accepted that permission
  gets no token at all (JN-FAIL-015), rather than a failure at this step.
- Covered by `TestMergeWhenGreenEnqueuesWithMergeQueue`.

---

### JN-MERGE-004: No check runs is not green

**Preconditions:** `merge = "on"`. The repository has no CI, so the head
commit has no check runs.

**Steps:**

1. Assign and label an issue. The review passes.

**Expected result:**

- The runner waits the whole `check_wait`.
- The reason is "the head commit has no check runs, so CI is not green after
  waiting 2m0s (check_wait)". Nothing is merged.

**Notes:**

- Covered by `TestFactoryMergeWithNoCheckRunsDoesNotMerge` and
  `TestMergeWhenGreenSaysHowLongItWaited`.

---

### JN-MERGE-005: A check that does not finish in time

**Preconditions:** `merge = "on"`. One check run is still `in_progress` when
`check_wait` ends.

**Steps:**

1. Assign and label an issue. The review passes.

**Expected result:**

- The reason is "check run <name> is still in_progress, so CI is not green
  after waiting <wait> (check_wait)".

**Notes:**

- Raise `check_wait` for slow CI (JN-CFG-010), and raise the runner's
  `--stuck-after` if the wait can be longer than it (JN-FAIL-014).
- Covered by `TestMergeWhenGreenSaysHowLongItWaited`.

---

### JN-MERGE-006: A red check gets one fix round, then merges

**Preconditions:** `merge = "on"`. The review passed with a round to spare
(for example on round 1 of 3). One check fails on the first head.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The runner reads the end of the failed check's log (the last 4000 bytes).
- A fresh worker session gets the check's name, its conclusion, and that log
  tail, and fixes the failure. The commit is "Apply review feedback for
  #<n>". The gate runs, and the same branch is pushed.
- Polling starts over on the new head inside the same `check_wait`. Green
  checks merge as in JN-MERGE-002.

**Notes:**

- The fix round counts against `--max-review-rounds`.
- Reading the log calls `GET /repos/{owner}/{repo}/actions/jobs/{job_id}/logs`,
  which needs Actions: read. The job token carries it.
- If the log cannot be read, the brief says "(schmeide could not read the log:
  ...)" instead of the tail.
- Covered by `TestCIFixRoundThenMerge`.

---

### JN-MERGE-007: Still red after the fix round

**Preconditions:** As JN-MERGE-006, but a check fails on the fixed head too.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- No second fix round. The reason is "check run <name> finished with failure,
  so CI is not green". The pull request stays open and the job is not
  retried.

**Notes:**

- Covered by `TestCIFixRoundRunsOnce` and `TestFactoryMergeWhenACheckFails`.

---

### JN-MERGE-008: A red check with no review round left

**Preconditions:** `merge = "on"`. The review passed on its last round (for
example round 3 of 3, or `--max-review-rounds 1`). A check fails.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- No fix round. The job fails at once with "check run <name> finished with
  failure, so CI is not green".

**Notes:**

- Covered by `TestCIFixSkippedWhenRoundsSpent` and
  `TestMergeWhenGreenDoesNotMergeRedChecks`.

---

### JN-MERGE-009: The CI fix round itself fails

**Preconditions:** As JN-MERGE-006, and the fix session fails, makes no
changes, or breaks the gate.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- Session failure or no changes: the reason is "check run <name> finished with
  failure, and the fix round failed: <error>", for example "...: the
  claude-code session made no changes". The job ends `merge_failed`.
- Gate failure: the issue gets "schmeide left the pull request open: the gate
  command `<gate>` failed: exit status 1. The gate's output is in the
  runner's log. <url>". The job ends `gate_failed`. The fix is not pushed.

**Notes:**

- No automated test for these cases.

---

### JN-MERGE-010: Branch protection requires approving reviews

**Preconditions:** `merge = "on"`. The base branch's protection requires one
or more approving reviews, and the runner's job token can read branch
protection.

**Steps:**

1. Assign and label an issue. The review passes.

**Expected result:**

- Before any check wait, the job fails with "branch protection on main
  requires 1 approving review(s), and schmeide cannot supply that review, so
  it did not merge; a person can approve and merge the pull request".
- Nothing is merged or enqueued.

**Notes:**

- Required status checks do not cause this. The check wait covers them.
- Covered by `TestMergeWhenGreenRefusesRequiredReviews` and
  `TestFactoryMergeRefusedByRequiredReviews`.

---

### JN-MERGE-011: Protection cannot be read (403 or 404)

**Preconditions:** `merge = "on"`. The branch has no protection (GitHub
answers 404), or the token may not read it (GitHub answers 403).

**Steps:**

1. Assign and label an issue. The review passes and checks go green.

**Expected result:**

- The runner treats the branch as unprotected and attempts the merge. GitHub
  still enforces its rules. If GitHub refuses, see JN-MERGE-013.

**Notes:**

- Reading protection needs the Administration: read permission. The control
  node mints each job's token with administration: read
  (`internal/control/token.go`, pinned by `TestTokenPerJob`), so against
  github.com a protected branch answers 200 and reaches JN-MERGE-010. A 403
  means the token was refused the read some other way; then a branch that
  requires approvals reaches JN-MERGE-013, not JN-MERGE-010.
- Covered by `TestMergeWhenGreenMergesAnUnprotectedBranch`.

---

### JN-MERGE-012: Reading protection fails for another reason

**Preconditions:** `merge = "on"`. The protection read returns a 5xx or a
network error.

**Steps:**

1. Assign and label an issue. The review passes.

**Expected result:**

- The job fails before the check wait with "could not read branch protection
  on main: <error>". Nothing is merged.

**Notes:**

- Covered by `TestProtectionFailsOnAServerError`.

---

### JN-MERGE-013: GitHub refuses the merge

**Preconditions:** `merge = "on"`. Checks are green, but GitHub refuses the
merge (for example a conflict, or a rule the runner could not see).

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The reason is "CI is green, but GitHub refused the merge: <GitHub's error>".

**Notes:**

- Every failure after the checks are green starts with "CI is green, but", so
  a pull request that looks ready says why it was not merged.
- Covered by `TestFactoryMergeRefusedByGitHub` and
  `TestMergeWhenGreenSaysWhyAGreenPullDidNotMerge`.

---

### JN-MERGE-014: A rejected review is never merged

**Preconditions:** `merge = "on"`. The review requests changes and either
`review_fix` is off or the rounds run out.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- No check wait and no merge. The outcome is JN-REV-004 or JN-REV-006.

**Notes:**

- Covered by `TestReviewFixOffWithMergeOnDoesNotMergeARejection` and
  `TestReviewExhaustingRoundsFailsAndDoesNotMerge`.

---

### JN-MERGE-015: A stacked pull request merges bottom first

**Preconditions:** `merge = "on"`. The pull request for issue 6 is stacked on
`5_a` (JN-PR-016). The pull request for issue 5 merges within `check_wait`.

**Steps:**

1. Assign and label issue 6. Its review passes.

**Expected result:**

- The runner waits for the pull request below to merge. While it is open,
  each poll rebases issue 6's branch onto `5_a` and force-pushes it with a
  lease when it moved.
- Once issue 5's pull request merged, issue 6's pull request is retargeted to
  the default branch (unless GitHub already did), rebased onto it, and
  force-pushed. Then the check wait runs and it merges as in JN-MERGE-002.

**Notes:**

- A bottom that was rebased and then merged still counts as merged. When
  GitHub does not report a merge time for the pull request below, the runner
  checks whether the base it last built on, or that pull request's latest head
  (`refs/pull/<n>/head`, which outlives a deleted branch, then the base
  branch), is in the default branch.
- Covered by `TestMergeWhenGreenMergesAStackBottomFirst`,
  `TestMergeWhenGreenRetargetsAndRebasesAfterTheBottomMerges`,
  `TestMergeWhenGreenRetargetsWhenTheBottomIsRebasedThenMerged`,
  `TestMergeWhenGreenRebasesWhenGitHubRetargeted`,
  `TestMergeWhenGreenFollowsAMovingBase`,
  `TestMergeWhenGreenRestacksARealBranchAfterTheBottomMerges`,
  `TestMergeWhenGreenRestacksAfterTheBottomIsRebasedAndMerged`.

---

### JN-MERGE-016: The bottom of the stack does not merge in time

**Preconditions:** As JN-MERGE-015, but issue 5's pull request is still open
when `check_wait` ends.

**Steps:**

1. Assign and label issue 6.

**Expected result:**

- The reason is "the pull request is stacked on <url of #5's pull request>,
  which has not merged into main after waiting <wait> (check_wait); schmeide
  merges a stack bottom first, so it did not merge this one".

**Notes:**

- Covered by `TestMergeWhenGreenWaitsForTheBottomOfAStack`.

---

### JN-MERGE-017: The bottom of the stack closed without merging

**Preconditions:** As JN-MERGE-015, but a person closed issue 5's pull request
without merging.

**Steps:**

1. Assign and label issue 6.

**Expected result:**

- The reason is "the pull request is stacked on 5_a, whose pull request for
  issue #5 closed without merging into main, so schmeide did not merge it; a
  person can retarget it to main".

**Notes:**

- A pull request below that was rebased and then closed unmerged still
  reads as closed: its latest head is not in the default branch.
- Covered by `TestMergeWhenGreenRefusesAStackOnAClosedPull` and
  `TestMergeWhenGreenRefusesAStackOnARebasedPullThatClosed`.

---

### JN-MERGE-018: A restack conflicts

**Preconditions:** As JN-MERGE-015, and rebasing issue 6's branch onto its new
base conflicts.

**Steps:**

1. Assign and label issue 6.

**Expected result:**

- The branch is left as it was. The reason is "could not restack the pull
  request onto <base>: could not rebase 6_b onto <base>, so schmeide left it
  as it was; a person has to rebase it: ...".

**Notes:**

- A secret found after the rebase refuses the push the same way as JN-FAIL-007.
- Covered by `TestMergeWhenGreenSaysWhenARestackConflicts`.

---

### JN-MERGE-019: A skipped check does not block the merge

**Preconditions:** `merge = "on"`. The head commit's check runs include at
least one that completed with conclusion `skipped`, and every run that is
not `skipped` completed with `success`. GitHub reports a path-filtered job,
and a matrix placeholder that did not run, that way. The name of a skipped
run can be `build-android`, `build-backend`, `build-linux`, or
`test-frontend (${{ matrix.name }})`.

**Steps:**

1. Assign and label an issue. The review passes.

**Expected result:**

- `judgeChecks` in `internal/runner/merge.go` treats `skipped` as green. The
  pull request merges as in JN-MERGE-002, or is added to the merge queue as
  in JN-MERGE-003.
- The issue comment does not say "finished with skipped, so CI is not green".

**Notes:**

- A run that is still queued or in progress stays pending, even when another
  run is `skipped`.
- A completed conclusion of `failure`, `cancelled`, `timed_out`, or empty is
  still red, and that run is the one named, even when an earlier run was
  `skipped`.
- A list of only `skipped` runs is green. An empty list is still not green
  (JN-MERGE-004).
- Covered by `TestJudgeChecks`.
