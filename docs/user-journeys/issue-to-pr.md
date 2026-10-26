# Issue to pull request journeys

Covers how an issue becomes a job and how the job becomes a pull request:
webhook intake on the control node (`internal/control/intake.go`,
`internal/control/control.go`, `internal/control/queue.go`) and the runner's
work up to the open pull request (`internal/runner/runner.go`,
`internal/runner/github.go`). What happens after the pull request opens is in
[review.md](review.md) and [merge.md](merge.md).

The App's **Advanced** tab (Recent Deliveries) shows each webhook's response
code and body. The body is one word: `created`, `duplicate`, `ignored`, or
`replay`.

---

### JN-PR-001: Assign, then label, to create a job

**Preconditions:** A control node (JN-SETUP-004) and a runner for steward
`octocat` (JN-SETUP-019). The repository is in an allowed org and has a valid
`.schmeide/config.toml` (JN-CFG-001).

**Steps:**

1. Open an issue in the repository.
2. Assign it to `octocat`.
3. Add the label `schmeide:ready`.

**Expected result:**

- The assignment delivery answers 200 `ignored` (no label yet).
- The label delivery answers 202 `created`.
- The control node logs `msg="webhook handled" delivery=<id> outcome=created`.
- The runner logs `msg="job state" repo=<owner>/<repo> issue=<n> state=claimed`,
  then `state=working detail=claude-code`.

**Notes:**

- A job needs all of: an `issues` event whose action is `labeled` or
  `assigned`, an open issue, an assignee, and the ready label.
- With several assignees, the first one is the steward.
- An edit or a reopen does not create a job, even when the issue is assigned
  and labeled: only someone with triage access can label or assign.
- Covered by `TestWebhookIntake` and
  `TestJourneyLabeledIssueOpensOnePullRequest` (`internal/e2e`).

---

### JN-PR-002: Label, then assign, to create a job

**Preconditions:** As JN-PR-001.

**Steps:**

1. Open an issue and add `schmeide:ready`.
2. Assign it to `octocat`.

**Expected result:**

- The label delivery answers 200 `ignored` (no assignee yet).
- The assignment delivery answers 202 `created`. The job runs as in JN-PR-001.

**Notes:**

- Covered by `TestWebhookIntake`.

---

### JN-PR-003: A repeated delivery is not a second job

**Preconditions:** JN-PR-001 done less than `--delivery-ttl` (default 10m)
ago.

**Steps:**

1. In the App's Advanced tab, choose **Redeliver** on the label delivery.

**Expected result:**

- The redelivery answers 200 `replay`. No job is published.
- The control node logs `msg="webhook handled" delivery=<id> outcome=replay`.

**Notes:**

- Delivery ids are remembered in memory, so a control node restart forgets
  them. After the TTL or a restart, the same delivery answers `duplicate`
  instead (JN-PR-004). Either way there is one job.
- A delivery that was rejected (401, 403, 429, 500) is not remembered, so its
  redelivery is handled again.
- The replay check runs after the signature check and the rate limit. A request
  that reuses a remembered delivery id without a valid `X-Hub-Signature-256`
  answers 401, not `replay`. A redelivery counts toward `--webhook-rate`, so
  over the limit it answers 429.
- Covered by `TestRepeatedDeliveryIsAcknowledgedWithoutPublishing`,
  `TestRejectedDeliveryIDIsNotRemembered`,
  `TestUnsignedRequestNeverReachesTheReplayCache`, and
  `TestReplayCountsAgainstTheRateLimit` in `internal/control`.

---

### JN-PR-004: More events on an issue that already has a job

**Preconditions:** An issue has a job that is queued, running, or finished
with a pull request (any state other than `failed` or `gate_failed`).

**Steps:**

1. Add another label to the issue, or remove and re-add `schmeide:ready`, or
   reassign it to another steward.

**Expected result:**

- The delivery answers 200 `duplicate`. No new job is published, whoever the
  new assignee is.

**Notes:**

- The control node keeps one record per issue in its data directory, so this
  holds across control node restarts.
- Covered by `TestDifferentDeliveryIDKeepsIdempotentJob` and
  `TestPublishRefusesAnUnfinishedOrSucceededIssue`.

---

### JN-PR-005: Relabel after a failed job to try again

**Preconditions:** The issue's last job ended `failed` (JN-FAIL-002) or
`gate_failed` (JN-PR-014). The cause is fixed.

**Steps:**

1. Remove `schmeide:ready` from the issue and add it again.

**Expected result:**

- The delivery answers 202 `created`. A new job runs from the start with the
  issue's current title and body.

**Notes:**

- Covered by `TestPublishReplacesAFailedJob`.

---

### JN-PR-006: Issues that do not become jobs

**Preconditions:** A control node.

**Steps:**

1. Add `schmeide:ready` to an issue with no assignee.
2. Assign an issue that has no ready label.
3. Assign and label an issue, then close it, then add another label.

**Expected result:**

- Each delivery answers 200 `ignored`. No job, no comment.

**Notes:**

- Any event other than `issues` (for example `ping`) also answers `ignored`
  once its signature checks out.
- Covered by `TestWebhookIntake`.

---

### JN-PR-007: An issue outside the allowlist is refused

**Preconditions:** The App is installed on an org that is not in
`--allowed-orgs`, and its installation is not in `--allowed-installations`.

**Steps:**

1. In that org, assign and label an issue.

**Expected result:**

- The delivery answers 403 with "webhook is from an org or installation that
  is not on the allowlist: <owner>/<repo> (installation <id>)".
- The control node logs `level=WARN msg="webhook rejected"` with that error.
  No job, no comment.

**Notes:**

- The allowlist is checked only for events that would create a job. Other
  deliveries from that org answer `ignored`.
- Covered by `TestWebhookAllowlist`.

---

### JN-PR-008: A delivery with a bad signature is refused

**Preconditions:** The App's webhook secret differs from the control node's
`--webhook-secret`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The delivery answers 401 with "webhook signature is missing or invalid".
  The control node logs `msg="webhook rejected"`.

**Notes:**

- A non-POST request to `/webhook` answers 405 "webhook deliveries must be
  POSTed". A body over 25 MiB answers 400 and publishes nothing.
- The signature is checked before the rate limit and the replay check. A
  request with a bad or missing signature answers 401 even when it reuses a
  delivery id the control node already handled, and it does not count toward
  `--webhook-rate`.
- Covered by `TestWebhookIntake`, `TestOversizedWebhookPublishesNothing`,
  `TestUnsignedRequestNeverReachesTheReplayCache`, and
  `TestUnsignedRequestsDoNotUseTheRateBudget` in `internal/control`.

---

### JN-PR-009: Too many deliveries in a minute

**Preconditions:** `--webhook-rate` (default 1000 per minute) is reached.

**Steps:**

1. Send one more delivery within the same minute.

**Expected result:**

- The delivery answers 429 "too many webhook requests". No job. GitHub's
  redelivery after the minute is handled normally.

**Notes:**

- Only deliveries with a valid signature count toward the limit. A redelivery
  of a delivery id already handled counts, and over the limit it answers 429
  rather than `replay`.
- Covered by `TestWebhookRateLimitPublishesNoJob` and
  `TestReplayCountsAgainstTheRateLimit` in `internal/control`.

---

### JN-PR-010: The job's branch name

**Preconditions:** JN-PR-001 with an issue numbered 12, titled
"Add a greeting: hello, World!".

**Steps:**

1. Wait for the pull request.

**Expected result:**

- The branch is `12_add-a-greeting-hello-world`: the issue number, `_`, and
  the title lowercased with every run of other characters turned into `-`,
  trimmed, and cut to 40 characters.

**Notes:**

- A title with no letters or digits gives `<n>_issue`.
- The runner finds an existing pull request by the `<n>_` prefix, so
  retitling the issue does not lead to a second pull request.
- Covered by `TestBranchName` and the e2e test's `1_add-a-greeting`.

---

### JN-PR-011: The brief uses the issue as it was when it became a job

**Preconditions:** A runner that is busy with another job, so the new job
waits.

**Steps:**

1. Assign and label an issue.
2. Before the runner takes it, edit the issue's title and body.

**Expected result:**

- The session works from the original title and body. The branch name also
  comes from the original title.
- To use the edited text, the job has to be recreated, which is possible only
  after it fails (JN-PR-005).

**Notes:**

- The brief tells the session the issue number, repository, title, body, and
  gate command, to follow the repository's conventions, not to push, and to
  write the pull request text to `.schmeide/pr.md`. A map of the
  repository's tracked files and Go symbols is appended.
- A job stored by an older control node without the issue's text fails with
  "the job does not carry the issue's text; remove the ready label and add it
  again to make a new job" (`TestJobWithoutIssueTextIsNotWorked`).
- No automated test for the edit-after-delivery case. The job carries
  `Title` and `Body` from the webhook payload (`internal/control/intake.go`).

---

### JN-PR-012: The gate passes and the pull request opens

**Preconditions:** JN-PR-001, with a gate that passes.

**Steps:**

1. Watch the repository's pull requests.

**Expected result:**

- The branch is pushed and a pull request opens, authored by the App
  (`schmeide[bot]`), from `<n>_<slug>` into the default branch.
- Commits are authored and committed as `schmeide[bot]
  <schmeide[bot]@users.noreply.github.com>` and are not signed.
- No comment is posted when the pull request opens. The runner logs
  `state=reviewing detail="round 1 of 3"` and the review starts
  (JN-REV-001). The outcome comment comes at the end of the job.

**Notes:**

- The order is: session, commit, gate, secret scan (JN-FAIL-007), push, open.
- `--git-name` and `--git-email` change the commit identity.
- Covered by `TestJobOpensPR` and `TestJourneyLabeledIssueOpensOnePullRequest`.

---

### JN-PR-013: The pull request title and body

**Preconditions:** JN-PR-012.

**Steps:**

1. Open the pull request.

**Expected result:**

- The title is the first line the session wrote to `.schmeide/pr.md`, and the
  body is the rest. The session is asked to follow the repository's title
  conventions and pull request template.
- The body contains `Closes #<n>`. If the session left it out, the runner
  appends it.
- `.schmeide/pr.md` is not in the pull request's files.
- If the session wrote no `.schmeide/pr.md`, the title is the issue title and
  the body is `Closes #<n>`.

**Notes:**

- Covered by `TestJobOpensPR` and the e2e journey (title `feat: add a
  greeting`).

---

### JN-PR-014: The gate fails

**Preconditions:** A gate that fails, for example `gate = "false"`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- No pull request. The branch is not pushed.
- The issue gets: "schmeide did not open a pull request: the gate command
  `false` failed: exit status 1. The gate's output is in the runner's log."
- The runner logs `level=WARN msg="gate failed"` with the last 4000 bytes of
  output, then `state=gate_failed`.
- The job is not retried.

**Notes:**

- The output stays out of the comment because the session wrote the code that
  produced it.
- Relabel to try again (JN-PR-005).
- Covered by `TestGateFailureOpensNoPR` and
  `TestJourneyGateFailureCommentsAndOpensNoPullRequest`.

---

### JN-PR-015: The session makes no changes

**Preconditions:** An issue the session decides needs no change.

**Steps:**

1. Assign and label the issue.

**Expected result:**

- Each attempt fails with "the claude-code session made no changes" and is
  retried. After `--max-attempts`, the issue gets "schmeide could not finish
  this job and did not open a pull request:" with that error.

**Notes:**

- A session that exits non-zero fails with "the claude-code session did not
  finish: ..." and is retried the same way.
- Covered by `TestFailedJobIsRetried` and `TestFailedJobIsFinalAfterMaxAttempts`.

---

### JN-PR-016: Stack on one open blocker

**Preconditions:** Issue 5 has an open pull request from schmeide on branch
`5_add-parser`. Issue 6 is marked as blocked by issue 5 in GitHub's issue
dependencies.

**Steps:**

1. Assign and label issue 6.

**Expected result:**

- The worktree starts from `5_add-parser`, and the pull request for issue 6
  opens with base `5_add-parser`.

**Notes:**

- A blocker counts when it is open and has an open pull request whose head
  branch starts with `<blocker number>_`.
- With `merge = "on"`, the stack merges bottom first (JN-MERGE-015).
- Covered by `TestOneOpenDependencyStacksThePullRequest`.

---

### JN-PR-017: No stackable blocker means the default branch

**Preconditions:** Issue 6 has no blockers, or every blocker is closed, or no
open blocker has an open `<number>_` pull request.

**Steps:**

1. Assign and label issue 6.

**Expected result:**

- The pull request opens against the repository's default branch.

**Notes:**

- A blocker without a schmeide pull request is skipped, even when another
  blocker can be stacked on.
- Covered by `TestNoDependencyOpensAgainstTheDefaultBranch`,
  `TestMergedDependencyOpensAgainstTheDefaultBranch`,
  `TestBlockerWithoutSchmeidePullRequestUsesDefaultBranch`,
  `TestOneStackableDependencyIgnoresAnUnstackableBlocker`.

---

### JN-PR-018: Stack on the top of a linear chain of blockers

**Preconditions:** Issue 7 is blocked by issues 5 and 6. `5_a` targets the
default branch, and `6_b` targets `5_a`. Both pull requests are open.

**Steps:**

1. Assign and label issue 7.

**Expected result:**

- The pull request for issue 7 opens with base `6_b`, the top of the chain.

**Notes:**

- Two or more stackable blockers are accepted when they form one chain. The
  order GitHub lists the blockers in does not matter.
- Covered by `TestLinearChainOfDependenciesStacksOnTheTop`.

---

### JN-PR-019: Blockers side by side fail the job

**Preconditions:** Issue 7 is blocked by issues 5 and 6, and both `5_a` and
`6_b` target the default branch.

**Steps:**

1. Assign and label issue 7.

**Expected result:**

- No worktree, no session, no pull request. The job reports `claimed` then
  `failed` on the first attempt and is not retried.
- The issue gets one comment: "schmeide could not finish this job and did not
  open a pull request:" with "the open dependencies' pull requests (5_a, 6_b)
  are not one linear stack, and schmeide stacks only on a single chain of
  blockers".

**Notes:**

- A retry cannot change the blocked-by list, so this failure is final, like a
  gate failure. A linear chain is accepted (JN-PR-018).
- Covered by `TestTwoSideBySideDependenciesOpenNothing`.

---

### JN-PR-020: Use a different ready label

**Preconditions:** The control node runs with `--ready-label factory`.

**Steps:**

1. Assign an issue and add `factory`.
2. On another issue, assign it and add `schmeide:ready`.

**Expected result:**

- Step 1 creates a job. Step 2 answers `ignored`.

**Notes:**

- Covered by `TestReadyLabelIsConfigurable`.

---

### JN-PR-021: The gate runs in a locked-down environment

**Preconditions:** A runner started with `SCHMEIDE_NATS_TOKEN`, an
`ANTHROPIC_API_KEY`, and `--gate-pass-env GOPATH`; a repo whose gate is
`env > "$TMPDIR/env.txt"; cat "$TMPDIR/env.txt"; test "$HOME" = "$TMPDIR"`.

**Steps:**

1. Assign and label an issue.
2. Start a second runner with `--gate-pass-env SCHMEIDE_NATS_TOKEN`.

**Expected result:**

- Step 1: the gate passes and the pull request opens (JN-PR-012). The gate's
  environment holds the base list (`PATH`, `USER`, `LANG`, the proxy and
  certificate variables, `LC_*`, `XDG_*`) and `GOPATH`, plus `GOCACHE`,
  `GOMODCACHE`, and `GOFLAGS`. It holds no `SCHMEIDE_*` name, no
  `ANTHROPIC_*`, `CLAUDE_*`, or `XAI_*` name, and no job token. `HOME` and
  `TMPDIR` are `<work-dir>/scratch/<repo_id>-<issue>`, and that directory is
  gone once the job ends.
- Step 2: the runner exits with code 2 and prints `schmeide runner:
  --gate-pass-env lists SCHMEIDE_NATS_TOKEN: the gate never sees a SCHMEIDE_
  name` and `Run 'schmeide runner --help' for usage.`

**Notes:**

- The gate still runs as the runner's OS user and can read that user's files
  by absolute path. See `docs/architecture/credentials.md`, "Gate
  environment".
- Covered by `TestGateEnvKeepsBaseAndGatePassEnvAndDropsSecrets`,
  `TestScratchHomeReplacesHomeTmpdirAndXDGDirs`,
  `TestGateRunsInAScratchHomeThatCloseRemoves`,
  `TestJobLocksGateGitAndHarnessEnvironments`, and
  `TestMainRefusesASchmeideGatePassEnv` in `internal/runner`. No e2e test.
