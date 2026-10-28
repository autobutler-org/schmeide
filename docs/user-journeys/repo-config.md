# Repo config journeys

Covers `.schmeide/config.toml`, the file a repo maintainer commits to tell
schmeide how to check and finish work in that repository. The full key
reference is [../config.md](../config.md). Implemented in
`internal/repoconfig`; the runner reads it in `internal/runner/runner.go`
(`execute`).

Every config error below reaches the issue the same way: the job fails
finally on its first attempt and is not retried, and the issue gets one comment
"schmeide could not finish this job and did not open a pull request:" followed
by the error. The error starts with the
config file's path inside the job's worktree, for example
`<work dir>/worktrees/<repo id>-<issue>/.schmeide/config.toml:`. Below, that
prefix is written `<file>:`. After a fix is pushed to the base branch, remove
and add the ready label again to make a new job.

---

### JN-CFG-001: Add a minimal config

**Preconditions:** The App is installed on the repository (JN-SETUP-002).

**Steps:**

1. Commit `.schmeide/config.toml` to the default branch with one line:

   ```toml
   gate = "make check test"
   ```

2. Assign and label an issue (JN-PR-001).

**Expected result:**

- The job runs. After the session, the runner runs `make check test` in the
  worktree with `sh -c`. When it passes, a pull request opens.
- Every optional key takes its default: `check_wait` 2m, the runner's
  `--gate-timeout` and `--session-timeout`, and every `[automate]` switch off.
  So the pull request is reviewed and left for a person to merge.

**Notes:**

- Every value is a quoted string.
- `schmeide init` writes this file (JN-SETUP-003).
- Covered by `TestLoad` and `TestJobOpensPR`.

---

### JN-CFG-002: No config file

**Preconditions:** The base branch has no `.schmeide/config.toml`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- No harness session runs and no pull request opens.
- The job reports `claimed` then `failed`, and is not retried.
- The issue gets one comment: "schmeide could not finish this job and did not
  open a pull request:" with "`<file>`: config file not found; create it with a
  gate command, e.g. gate = "make check test"".

**Notes:**

- A retry reads the same base branch, so a missing file is final on the first
  attempt.
- Covered by `TestMissingConfigIsFinal` (`internal/runner`) and `TestLoad`
  (`internal/repoconfig`).

---

### JN-CFG-003: Config with no gate

**Preconditions:** `.schmeide/config.toml` exists with no `gate`, or with
`gate = ""` or `gate = "   "`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The issue comment names "`<file>`: gate is missing or
  empty; set gate = "<command>", e.g. gate = "make check test"".

**Notes:**

- Covered by `TestLoad`.

---

### JN-CFG-004: Unknown key

**Preconditions:** The file has a typo, for example `gaet = "make test"`, or
an unknown key inside `[automate]`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The comment names "`<file>`: unknown key(s) gaet; remove them or fix the
  spelling". An unknown key in the table is named with its table, for example
  `automate.mrege`.

**Notes:**

- Unknown keys are an error so a typo fails loudly. The file is not partly
  applied.
- Covered by `TestUnknownKeyDoesNotClaimOnlyGateIsSupported` and `TestLoad`.

---

### JN-CFG-005: The old top-level `merge` key

**Preconditions:** The file sets `merge = "on"` at the top level.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The comment names "`<file>`: merge moved into [automate]; set it as
  [automate] merge = "on"".

**Notes:**

- Covered by `TestAutomate`.

---

### JN-CFG-006: A value that is not a quoted string

**Preconditions:** The file sets, for example, `check_wait = 10` or
`[automate]` `merge = true`, or sets `automate = "on"`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- `check_wait = 10`: "`<file>`: check_wait must be a quoted string; set it like
  check_wait = "10m"".
- `merge = true` in `[automate]`: "`<file>`: automate.merge must be a quoted
  string; set it like automate.merge = "on"".
- `automate = "on"`: "`<file>`: automate must be a table; set it like
  [automate] merge = "on"".

**Notes:**

- Other TOML syntax errors give "`<file>`: malformed TOML, fix the syntax: ...".
- Covered by `TestNonStringValueNamesKey`.

---

### JN-CFG-007: Set `gate_timeout`

**Preconditions:** The runner was started with the default `--gate-timeout`
(30m), or with `--gate-timeout 1h`.

**Steps:**

1. Set `gate_timeout = "10m"` in the config.
2. Assign and label an issue whose gate takes longer than 10 minutes.

**Expected result:**

- The gate is canceled at 10 minutes, whatever the runner flag says, and the
  issue gets "schmeide did not open a pull request: the gate timed out."
  (JN-FAIL-005).
- With the key absent or `gate_timeout = ""`, the runner's `--gate-timeout`
  applies.

**Notes:**

- The repo value wins over the flag when set.
- Covered by `TestTimeoutSelection` and
  `TestDeadlinesFollowConfigThenFlagThenDefault`.

---

### JN-CFG-008: Set `session_timeout`

**Preconditions:** A runner with the default `--session-timeout` (2h).

**Steps:**

1. Set `session_timeout = "30m"` in the config.
2. Assign and label an issue.

**Expected result:**

- Each harness session for jobs in this repository (writer, reviewer, and fix
  sessions) is canceled after 30 minutes (JN-FAIL-004).
- With the key absent or empty, the runner's `--session-timeout` applies.

**Notes:**

- Covered by `TestTimeoutSelection` and
  `TestDeadlinesFollowConfigThenFlagThenDefault`.

---

### JN-CFG-009: A timeout or wait that is not a positive duration

**Preconditions:** The file sets `check_wait = "soon"`, `gate_timeout = "0s"`,
or `session_timeout = "-5m"`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The comment names one of:
  - "`<file>`: check_wait = "soon" is not a positive duration; set a duration
    such as check_wait = "10m""
  - "`<file>`: gate_timeout = "0s" is not a positive duration; set a duration
    such as gate_timeout = "30m""
  - "`<file>`: session_timeout = "-5m" is not a positive duration; set a
    duration such as session_timeout = "30m""

**Notes:**

- Values are Go durations: `90s`, `10m`, `1h30m`.
- Covered by `TestCheckWait` and `TestTimeoutSelection`.

---

### JN-CFG-010: Set `check_wait`

**Preconditions:** `[automate] merge = "on"`.

**Steps:**

1. Set `check_wait = "10m"`.
2. Assign and label an issue.

**Expected result:**

- `check_wait` is how long a stacked pull request waits for the one below it
  (JN-MERGE-016). It does not end the wait for this pull request's own check
  runs. That wait runs until the pull request has merged (JN-MERGE-002,
  JN-MERGE-004). A completed check whose conclusion is `skipped` counts as
  success (JN-MERGE-019).

**Notes:**

- Absent or empty means `2m`. The key has no effect with merge off.
- Covered by `TestCheckWait` and `TestMergeWhenGreenStaysWhileChecksArePending`.

---

### JN-CFG-011: `[automate]` switches default to off

**Preconditions:** None.

**Steps:**

1. Commit a config with no `[automate]` table, or with `merge = ""`,
   `merge = "  "`, or `merge = "off"`.
2. Assign and label an issue whose review requests changes.

**Expected result:**

- The review requests changes, no fix round runs, and the pull request is left
  open for a person (JN-REV-004).
- A pull request whose review passes is left open for a person to merge
  (JN-MERGE-001).

**Notes:**

- Covered by `TestAutomate`.

---

### JN-CFG-012: An illegal switch value

**Preconditions:** The file sets `[automate]` `merge = "yes"` or `merge = "On"`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- The comment names "`<file>`: automate.merge = "yes" is not a legal value; set
  automate.merge = "on" or automate.merge = "off"".

**Notes:**

- Values are case-sensitive. Only `"on"` and `"off"` (and empty) are legal.
- Covered by `TestAutomate` and `TestAutomateMergeNopeIsAConfigError`.

---

### JN-CFG-013: Turn on `automate.merge`

**Preconditions:** None.

**Steps:**

1. Add:

   ```toml
   [automate]
   merge = "on"
   ```

2. Assign and label an issue.

**Expected result:**

- After the review passes, the runner waits for green checks and merges the
  pull request with a merge commit, or adds it to the merge queue. The issue
  gets "schmeide merged the pull request after the review passed and CI
  succeeded: <url>" (JN-MERGE-002).

**Notes:**

- See [merge.md](merge.md) for every outcome.

---

### JN-CFG-014: Turn on `automate.review_fix`

**Preconditions:** None.

**Steps:**

1. Add:

   ```toml
   [automate]
   review_fix = "on"
   ```

2. Assign and label an issue whose first review requests changes.

**Expected result:**

- A fresh worker session applies the findings, the gate runs again, and the
  same branch is pushed so the open pull request updates (JN-REV-005).

---

### JN-CFG-015: `automate.create_issues` has no effect

**Preconditions:** None.

**Steps:**

1. Set `[automate]` `create_issues = "on"`.
2. Assign and label an issue.

**Expected result:**

- The file is accepted. The job behaves the same as with the switch off.
  schmeide creates no issues.

**Notes:**

- The switch is validated like the others (JN-CFG-012) but nothing reads it.
- Covered by `TestAutomate`.

---

### JN-CFG-016: The session cannot change its own config

**Preconditions:** The base branch's config sets `gate = "make check test"`
and `merge = "off"`.

**Steps:**

1. Assign and label an issue whose change also edits `.schmeide/config.toml`
   (for example to `gate = "true"` and `merge = "on"`).

**Expected result:**

- The job runs `make check test`, not `true`, and does not merge. The new
  config is part of the pull request's diff, for a person to review.

**Notes:**

- The runner reads the config from the freshly created worktree, at the base
  branch, before the harness session starts. The gate, the timeouts,
  `check_wait`, and the `[automate]` switches all come from that one read.
- For a stacked pull request the base is the blocker's branch (JN-PR-016), so
  the config is read from there.
- For a job resumed at review after a restart (JN-FAIL-010), the config is read
  from the base before the branch moves to the pull request's head.
- No automated test for the edited-config case.

---

### JN-CFG-017: Warning when the gate skips the repo's own lint gate

**Preconditions:** The repository has a `Makefile` with a `check` target, or a
pre-commit config, and the gate does not mention `check` (or `pre-commit`),
for example `gate = "go test ./..."`.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- Before the session, the issue gets: "schmeide detected Makefile target
  `check`. The configured gate is `go test ./...`, which does not mention
  `check`. This is a warning; the job still runs."
- The job continues as usual.

**Notes:**

- With both found and both missed, the comment names both, joined with "or".
- The comment is posted on each attempt.
- Implemented in `internal/lintgate`. Covered by
  `TestLintGateMismatchCommentsWithoutFailingTheJob` and
  `TestWarningNamesDetectedTargetAndGate`.
