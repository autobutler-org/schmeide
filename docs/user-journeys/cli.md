# CLI journeys

Covers the `schmeide` command itself: help, settings from the environment,
`status`, `version`, `completion`, and `init`. The commands are wired in
`cmd/schmeide`. `control` and `status` are in `internal/control`, `runner` in
`internal/runner`, and `doctor` in `internal/doctor`.

Exit codes: 0 for success and help, 2 for a usage error (unknown flag,
missing required flag, bad setting), and 1 when the command ran and failed.

---

### JN-CLI-001: Show top-level help

**Preconditions:** `schmeide` is on `PATH`.

**Steps:**

1. Run `schmeide`, `schmeide -h`, or `schmeide --help`.

**Expected result:**

- Exit code 0. Stdout lists the commands `control`, `runner`, `doctor`,
  `status`, `init`, `completion`, and `version`, each with one line, then
  "Run 'schmeide <command> --help' for a command's flags." and "See
  docs/GLOSSARY.md for what the terms mean.".

**Notes:**

- Covered by `TestRun` and `TestUsageLinksGlossary`.

---

### JN-CLI-002: An unknown command or flag

**Preconditions:** None.

**Steps:**

1. Run `schmeide nope`.
2. Run `schmeide runner --nope`.

**Expected result:**

- Step 1: exit code 2, "schmeide: unknown argument "nope"" and "Run 'schmeide
  --help' for usage.".
- Step 2: exit code 2, an error naming the flag and "Run 'schmeide runner
  --help' for usage.". Each command names its own help the same way.

**Notes:**

- A stray positional argument fails with "unexpected argument "x"".
- Covered by `TestRun` and `TestMainExitCodes`.

---

### JN-CLI-003: Show a command's help

**Preconditions:** None.

**Steps:**

1. Run `schmeide control --help`, `schmeide runner --help`,
   `schmeide doctor --help`, `schmeide status --help`, `schmeide init --help`,
   `schmeide completion --help`, and `schmeide version --help`.

**Expected result:**

- Exit code 0 for each. The help explains what the command does and lists
  every flag with its environment variable and default, for example:

  ```text
    --max-attempts
          How many times a job's work may fail before that failure is final ...
          Env: SCHMEIDE_MAX_ATTEMPTS (default "3")
  ```

- `control --help` marks required settings with `(required)`.
- `runner --help` explains the job flow, review rounds, factory merge,
  timeouts, heartbeats, the permission floor, the environment a session sees,
  and the trust prompt.

**Notes:**

- Covered by `TestRun`, `TestControlUsageDocumentsEverySetting`,
  `TestRunnerHelpDocumentsReviewRounds`, `TestUsageStatesPermissionFloor`,
  `TestUsageNamesTrust`, `TestUsageDescribesTheGateEnvironment`.

---

### JN-CLI-004: Set any flag from the environment

**Preconditions:** None.

**Steps:**

1. Take a flag name, for example `--max-review-rounds`.
2. Uppercase it, replace `-` with `_`, and prefix `SCHMEIDE_`:
   `SCHMEIDE_MAX_REVIEW_ROUNDS`.
3. Run `SCHMEIDE_MAX_REVIEW_ROUNDS=1 schmeide runner --steward octocat`.

**Expected result:**

- The runner uses 1 review round, as if `--max-review-rounds 1` was passed.

**Notes:**

- The rule holds for every flag of `control`, `runner`, and `doctor`,
  including booleans (`SCHMEIDE_POOL=true`). `status` reads only
  `SCHMEIDE_STEWARD`, `SCHMEIDE_NATS_URL`, and `SCHMEIDE_NATS_TOKEN`.
- `SCHMEIDE_TRUST` is not a flag. It is read by the runner directly
  (JN-SETUP-023).
- An empty variable counts as unset.
- Covered by `TestRun` (`set SCHMEIDE_GITHUB_APP_ID` in the error text) and
  `TestParseControl`.

---

### JN-CLI-005: Precedence on `control`: flag, then environment, then file

**Preconditions:** A config file with `http-addr = :7070`.

**Steps:**

1. Run `schmeide control --config <file>` with every required setting.
2. Add `SCHMEIDE_HTTP_ADDR=:8081`.
3. Add `--http-addr :8082`.

**Expected result:**

- The startup log's `webhook=` address is `:7070`, then `:8081`, then `:8082`.

**Notes:**

- The file is named by `--config` or `SCHMEIDE_CONFIG`.
- Same as JN-SETUP-005. Covered by `TestParseControl`.

---

### JN-CLI-006: Precedence on `runner`, `doctor`, and `status`: flag, then environment

**Preconditions:** None.

**Steps:**

1. Run `SCHMEIDE_NATS_URL=nats://127.0.0.1:1 schmeide doctor --steward octocat`.
2. Run the same with `--nats-url nats://127.0.0.1:4222` added.

**Expected result:**

- Step 1 tries `nats://127.0.0.1:1`. Step 2 tries `nats://127.0.0.1:4222`: the
  flag wins.

**Notes:**

- These commands take no config file.
- An environment value that does not parse for its flag (for example
  `SCHMEIDE_MAX_ATTEMPTS=lots`) is ignored, and the flag's default is used. A
  flag value that does not parse is an error, exit code 2.
- Implemented in `runner.ParseFlags` (`internal/runner/cli.go`). No automated
  test for the ignored bad value.

---

### JN-CLI-007: See registered runners and running jobs

**Preconditions:** A control node with a registered runner and a job in
progress.

**Steps:**

1. Run `schmeide status --steward octocat --nats-token <token>`.

**Expected result:**

- Exit code 0. Output:

  ```text
  RUNNER    HOST         HARNESSES         POOL
  octocat   octo-laptop  claude-code,grok  false

  JOB                          STATE      FOR  DETAIL
  schmeide.jobs.octocat.42.7   reviewing  4m2s round 1 of 3
  ```

- The JOB column is `schmeide.jobs.<steward>.<repo id>.<issue>`, or
  `schmeide.jobs._pool.<repo id>.<issue>` for a pool job.
- FOR is how long the job has been in its state: the time since its latest
  status was stored on `SCHMEIDE_STATUS`, rounded to the second.

**Notes:**

- Jobs whose latest state is final (`pr_opened`, `gate_failed`, `failed`,
  `review_failed`, `merged`, `queued`, `merge_failed`) are not listed.
- A job no runner has claimed yet is listed as `pending` (JN-CLI-016).
- Status is team-wide and read-only: any steward sees every runner and every
  steward's jobs, pool jobs included, and running it writes nothing.
- A stopped runner stays listed.
- Covered by `TestStatusListsRegisteredRunnerAndQueuedJob` and
  `TestStatusHidesEveryFinishedRunnerState`.

---

### JN-CLI-008: `schmeide status` with nothing to show

**Preconditions:** A fresh control node.

**Steps:**

1. Run `schmeide status --steward octocat --nats-token <token>`.

**Expected result:**

- Exit code 0. Output:

  ```text
  No registered runners.

  No queued or running jobs.
  ```

**Notes:**

- Covered by `TestStatusWithAnEmptyStreamSaysThereAreNoJobs`.

---

### JN-CLI-009: `schmeide status --json`

**Preconditions:** As JN-CLI-007 or JN-CLI-008.

**Steps:**

1. Run `schmeide status --steward octocat --nats-token <token> --json`.

**Expected result:**

- One JSON object with `runners` (each with `steward`, `harnesses`, `host`,
  and `pool` and `version` when set) and `jobs` (each with `subject`,
  `state`, `detail` when set, and `since`, the RFC 3339 time the job entered
  its state). Empty lists are `[]`. A pending job is in `jobs` like any other
  (JN-CLI-016).

**Notes:**

- The JSON shape is stable.
- Covered by `TestStatusListsRegisteredRunnerAndQueuedJob`,
  `TestStatusWithAnEmptyStreamSaysThereAreNoJobs`, and
  `TestStatusListsAJobNoRunnerHasClaimed` (`internal/control`).

---

### JN-CLI-010: `schmeide status` fails

**Preconditions:** None.

**Steps:**

1. Run `schmeide status` with no steward.
2. Run `schmeide status --steward octocat --nats-url nats://127.0.0.1:1`.

**Expected result:**

- Step 1: exit code 2, "schmeide status: missing --steward: pass your GitHub
  login with --steward or set SCHMEIDE_STEWARD".
- Step 2: exit code 1, "schmeide status: cannot reach the control node's NATS
  at nats://127.0.0.1:1 as octocat: ..." and "Check that `schmeide control`
  is running, that --nats-url points at its --nats-addr, and that
  --nats-token is the token its --runner-tokens lists for octocat.".

**Notes:**

- Covered by `TestRun`.

---

### JN-CLI-011: Print the version

**Preconditions:** A binary built with `make build` at tag `v0.0.1`, or with
`go run`/`go install`.

**Steps:**

1. Run `schmeide version`.
2. Run `schmeide version extra`.

**Expected result:**

- Step 1: exit code 0. Stdout is `v0.0.1` for the tagged build, the output of
  `git describe --tags --always --dirty` for other `make build` builds, and
  `dev` for an unstamped build.
- Step 2: exit code 2, "schmeide version: takes no arguments".

**Notes:**

- See [../VERSIONING.md](../VERSIONING.md).
- Covered by `TestVersionUnstamped`.

---

### JN-CLI-012: Print a completion script

**Preconditions:** None.

**Steps:**

1. Run `schmeide completion bash`, `zsh`, or `fish`.
2. Run `schmeide completion tcsh`, or `schmeide completion` with no shell.

**Expected result:**

- Step 1: exit code 0, the script on stdout. Installing it is JN-SETUP-026 to
  JN-SETUP-028.
- Step 2: exit code 2, "schmeide completion: expected one of bash, zsh, fish"
  and "usage: schmeide completion bash|zsh|fish".

**Notes:**

- Covered by `TestCompletion` and `TestCompletionUnknownShell`.

---

### JN-CLI-013: `schmeide init` writes both files

**Preconditions:** Neither file exists.

**Steps:**

1. Run `schmeide init`.

**Expected result:**

- Same as JN-SETUP-003: both files written with mode 0600, and the "Next:"
  line. Exit code 0.

**Notes:**

- `schmeide init extra` fails with "schmeide init: unexpected argument
  "extra"", exit code 2.
- Covered by `TestInitWritesBothFiles`.

---

### JN-CLI-014: `schmeide init` refuses to overwrite

**Preconditions:** `.schmeide/config.toml` or `.schmeide/control.conf`
already exists.

**Steps:**

1. Run `schmeide init`.

**Expected result:**

- Exit code 1. Stderr: "schmeide init: .schmeide/config.toml already exists;
  remove it or edit it by hand, nothing was changed" (naming whichever file
  exists first).
- Neither file is changed, and the missing one is not created.

**Notes:**

- Covered by `TestInitRefusesToOverwrite`.

---

### JN-CLI-015: `schmeide doctor`

**Preconditions:** None.

**Steps:**

1. Run `schmeide doctor --steward <login> --nats-token <token>`.

**Expected result:**

- See JN-SETUP-010 (pass) and JN-SETUP-011 to JN-SETUP-017 (each failure and
  `--fix`). Exit code 0 when ready, 1 when not.

**Notes:**

- `--json` writes `[{"name", "group", "status", "detail"}, ...]` to stdout.
  `status` is `ok`, `fail`, or `skip`.
- Covered by `TestMainJSON` and `TestMainHelpAndBadFlag`.

---

### JN-CLI-016: See jobs no runner has claimed yet

**Preconditions:** A control node. octocat's runner works one job at a time
and is busy with an earlier job, or is stopped. Five issues are assigned to
octocat and labeled `schmeide:ready`.

**Steps:**

1. Run `schmeide status --steward octocat --nats-token <token>`.
2. Let the runner claim one of the waiting jobs, and run it again.
3. Restart the control node, and run it again.

**Expected result:**

- Step 1: every job the control node created and no runner has reported on
  is listed with state `pending`, and FOR is how long it has waited:

  ```text
  JOB                          STATE    FOR    DETAIL
  schmeide.jobs.octocat.42.8   pending  3m10s
  schmeide.jobs.octocat.42.9   pending  3m10s
  ```

  A job for a steward with no registered runner waits on the pool subject,
  `schmeide.jobs._pool.<repo id>.<issue>`, and is listed the same way.
- Step 2: the claimed job shows `claimed`, and FOR restarts from the claim.
  The others stay `pending` with their wait unchanged.
- Step 3: the pending jobs are still listed with the same wait, since
  `SCHMEIDE_STATUS` is kept in `--data-dir`.
- `--json` lists them in `jobs` with `"state": "pending"` and `since`.
- Status is team-wide and read-only: hubot running `schmeide status --steward
  hubot` sees octocat's pending jobs and pool jobs too, and changes nothing.

**Notes:**

- The control node stores the `pending` status in `Queue.publishPending`
  (`internal/control/queue.go`) when it creates a job, before it publishes the
  job, so a runner's `claimed` is always stored after it.
- A duplicate delivery for the issue creates no job and stores no second
  `pending`, so the wait is not reset.
- If the `pending` status cannot be stored, the job is still created and the
  control node logs `could not record the job as pending; schmeide status will
  not list it until a runner claims it`. If the job itself cannot be stored,
  the `pending` status is deleted again.
- A job a runner hands back (a failed token request, a stuck job, a shutdown)
  shows the runner's last state, such as `claimed`, until the next runner
  reports on it. The job is back in the queue, but it is not shown as
  `pending` again.
- Covered by `TestStatusListsAJobNoRunnerHasClaimed`,
  `TestPendingJobSurvivesARestart`, `TestPendingStatusFailureKeepsTheJob`, and
  `TestFailedPublishLeavesNoPendingJob` (`internal/control`).
