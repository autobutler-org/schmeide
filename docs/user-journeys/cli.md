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
  `status`, `dashboard`, `init`, `completion`, and `version`, each with one line, then
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
   `schmeide doctor --help`, `schmeide status --help`,
   `schmeide dashboard --help`, `schmeide init --help`,
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
  including booleans (`SCHMEIDE_POOL=true`). `status` and `dashboard` read
  only `SCHMEIDE_STEWARD`, `SCHMEIDE_NATS_URL`, and `SCHMEIDE_NATS_TOKEN`.
- `SCHMEIDE_TRUST` is not a flag. It is read by the runner directly
  (JN-SETUP-023).
- An empty variable counts as unset.
- On `runner` and `doctor`, a variable whose value does not parse for its
  flag's type (a duration, an integer, a boolean) is a usage error, exit code
  2, unless the flag was also given (JN-CLI-006).
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
3. Run `SCHMEIDE_ACK_WAIT="5 minutes" schmeide runner --steward octocat`.
4. Run `SCHMEIDE_JSON=maybe schmeide doctor`.
5. Run step 3 again with `--ack-wait 5m` added.

**Expected result:**

- Step 1 tries `nats://127.0.0.1:1`. Step 2 tries `nats://127.0.0.1:4222`: the
  flag wins.
- Step 3: exit code 2, nothing is started, and stderr is:

  ```text
  schmeide runner: SCHMEIDE_ACK_WAIT is "5 minutes", which is not a duration; set it to one such as 5m, unset it, or pass --ack-wait
  Run 'schmeide runner --help' for usage.
  ```

- Step 4: exit code 2, no checks run, and stderr is:

  ```text
  schmeide doctor: SCHMEIDE_JSON is "maybe", which is not a boolean; set it to true or false, unset it, or pass --json
  Run 'schmeide doctor --help' for usage.
  ```

- Step 5: the runner starts with an ack wait of 5m. A given flag means its
  variable is not read, so the bad value is not an error.

**Notes:**

- These commands take no config file.
- The message depends on the flag's type. A duration flag says "which is not a
  duration; set it to one such as 5m". An integer flag, such as
  `--max-attempts`, says "which is not an integer; set it to one such as 3". A
  boolean flag, such as `--pool`, says "which is not a boolean; set it to true
  or false". String flags accept any value.
- When several variables are bad, each gets its own line, in flag order.
- A flag value that does not parse is also an error, exit code 2 (JN-CLI-002).
- `control` takes every setting as a string and checks `--webhook-rate` and
  `--delivery-ttl` itself, with exit code 2 (JN-SETUP-009). `status` takes only
  strings.
- Implemented in `runner.ParseFlags` (`internal/runner/cli.go`), which `runner`
  and `doctor` share. Covered by `runner.TestMainRejectsUnparsableEnv`,
  `runner.TestParseFlagsFlagWinsOverUnparsableEnv`, and
  `doctor.TestMainRejectsUnparsableEnv`.

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
  `review_failed`, `merged`, `queued`, `merge_failed`) are not listed. The
  runner does not report `queued`; a factory merge stays `reviewing` until
  the pull request has merged (JN-MERGE-003). A recorded `queued` state, from
  an older runner, is still hidden.
- A job no runner has claimed yet is listed as `pending` (JN-CLI-016).
- A steward sees only their own runners and their own jobs. A steward with a
  live `--pool` runner also sees pool jobs. Other stewards' runners, hosts,
  versions, and job details are never shown. Running it writes nothing.
- A runner that has not refreshed its registration for three minutes is not
  listed (JN-POOL-009).
- Covered by `TestStatusListsRegisteredRunnerAndQueuedJob`,
  `TestStatusShowsOnlyTheAskingStewardsRunnersAndJobs`, and
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
  `state`, `detail` when set, `repo` and `title` when a status named them,
  and `since`, the RFC 3339 time the job entered its state). There is no
  `finished` key; only a query that asks for finished jobs, as the dashboard
  does, gets one (JN-CLI-017). Empty lists are `[]`. A pending job is in `jobs` like any other
  (JN-CLI-016).

**Notes:**

- The JSON shape is stable. `repo` and `title` were added for #264: the
  control node's `pending` status and every status from a runner of #264 or
  later carry them, and a status that names neither keeps the ones before it.
  A job reported only by an older runner, after a control node restart, has
  neither.
- Covered by `TestStatusCarriesRepoAndTitle`,
  `TestStatusQueryListsFinishedJobsOnRequest`,
  `TestStatusListsRegisteredRunnerAndQueuedJob`,
  `TestStatusWithAnEmptyStreamSaysThereAreNoJobs`, and
  `TestStatusListsAJobNoRunnerHasClaimed` (`internal/control`).

---

### JN-CLI-010: `schmeide status` fails

**Preconditions:** None.

**Steps:**

1. Run `schmeide status` with no steward.
2. Run `schmeide status --steward octocat --nats-url nats://127.0.0.1:1`.
3. Run it against a control node from before status queries.

**Expected result:**

- Step 1: exit code 2, "schmeide status: missing --steward: pass your GitHub
  login with --steward or set SCHMEIDE_STEWARD".
- Step 2: exit code 1, "schmeide status: cannot reach the control node's NATS
  at nats://127.0.0.1:1 as octocat: ..." and "Check that `schmeide control`
  is running, that --nats-url points at its --nats-addr, and that
  --nats-token is the token its --runner-tokens lists for octocat.".
- Step 3: exit code 1, "schmeide status: the control node does not answer
  status queries; upgrade `schmeide control` to this version".

**Notes:**

- Steps 1 and 2 are covered by `TestRun`; step 3 by `internal/control`
  `TestStatusAsksTheControlNode`.

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

- All three scripts list every command `run` in `cmd/schmeide/main.go`
  dispatches: `control`, `runner`, `doctor`, `status`, `dashboard`, `init`,
  `completion`, and `version`. `TestCompletionListsEveryCommand` reads the commands from
  that switch, so a command added to the dispatcher and not to every script
  fails it.
- Covered by `TestCompletion`, `TestCompletionListsEveryCommand`, and
  `TestCompletionUnknownShell`.

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
- A `SCHMEIDE_<NAME>` value that does not parse for its flag is a usage error,
  exit code 2 (JN-CLI-006).
- Covered by `TestMainJSON`, `TestMainHelpAndBadFlag`, and
  `TestMainRejectsUnparsableEnv`.

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

  A job for a steward with no live registration waits on the pool subject,
  `schmeide.jobs._pool.<repo id>.<issue>`, and is listed the same way for a
  steward with a live `--pool` runner.
- Step 2: the claimed job shows `claimed`, and FOR restarts from the claim.
  The others stay `pending` with their wait unchanged.
- Step 3: the pending jobs are still listed with the same wait, since
  `SCHMEIDE_STATUS` is kept in `--data-dir`.
- `--json` lists them in `jobs` with `"state": "pending"` and `since`.
- Status is scoped to the asking steward (JN-CLI-007): hubot running
  `schmeide status --steward hubot` does not see octocat's pending jobs, and
  sees pending pool jobs only with a live `--pool` runner. Running it changes
  nothing.

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

---

### JN-CLI-017: Watch the factory on the dashboard

**Preconditions:** A control node. octocat has a runner registered and a NATS
token. Issues assigned to octocat have been labeled `schmeide:ready`, and some
of their jobs have finished in the last day.

**Steps:**

1. Run `schmeide dashboard --steward octocat --nats-token <token>`.
2. Open `http://127.0.0.1:7878`.
3. Label another issue, and watch the page while the runner works it.
4. Click a job in the line or the jobs table.

**Expected result:**

- Step 1: stderr logs `msg="dashboard serving" url=http://127.0.0.1:7878
  steward=octocat model=sonnet`, and the command keeps running until Ctrl-C.
- Step 2: the header shows `control node connected`, the steward, this
  binary's version, and how long ago the page last refreshed. Below it:
  - totals: active jobs (with how many are working, reviewing, and retrying),
    pending jobs and the oldest wait, live runners and how many run with
    `--pool`, and jobs finished in the last 24 hours (opened or merged, and
    failed);
  - the line: one column each for `pending`, `working` (`claimed` and
    `working`), `retrying`, `reviewing`, `opened · merged` (`pr_opened`,
    `merged`, `queued`), and `failed` (`gate_failed`, `failed`,
    `review_failed`, `merge_failed`), each job as a chip with its issue number,
    repository name, and issue title;
  - the jobs table, newest state first, with the repository, issue number,
    title, state, how long the job has been in that state, and the detail. A
    pull request URL in the detail is a link. The filters are `Active`
    (unfinished jobs, the default), `Needs a look` (`retrying` and the failed
    states), `Finished` (the last 24 hours), and `All`;
  - runners: host, steward (or `pool`), harnesses, and version. A version that
    differs from this binary's is highlighted;
  - the activity log: each state the dashboard has seen, newest first, at the
    time the control node stored it.
- Step 3: within `--interval` (5s) plus the page's 3-second refresh, the new
  job appears as `pending`, then moves through the line as its state changes,
  and each state it was seen in is added to the activity log.
- Step 4: the job is highlighted, and a bar under the table shows its subject,
  state, and age, an "Ask about this job" button, and an "Open issue" link to
  `https://github.com/<repo>/issues/<issue>`.

**Notes:**

- The dashboard sends `{"finished": true}` on
  `schmeide.status-query.<steward>`, so it sees exactly what `schmeide status`
  sees (JN-CLI-007: the steward's own runners and jobs, plus pool jobs with a
  live `--pool` runner) and the same jobs that finished within 24 hours.
- The activity log is in memory. A restarted dashboard starts with one event
  per unfinished and recently finished job. A state that came and went
  between two polls has no event.
- A job with no repo or title (an older runner, after a control node restart)
  shows `repo <repo id>` and no title, and has no issue link.
- The page is served only to `localhost` and loopback addresses unless
  `--addr` is not loopback; see JN-CLI-019.
- Covered by `TestJourneyDashboardShowsTheStewardsJobsAndAnswers`
  (`internal/e2e`), `TestRunServesTheStewardsJobsAndAnswers`,
  `TestBoardUpdateParsesJobsAndRunners`, `TestBoardActivity`,
  `TestBoardActivityKeepsTheNewest` (`internal/dashboard`), and
  `TestStatusQueryListsFinishedJobsOnRequest` (`internal/control`). The page's
  JavaScript has no automated test.

---

### JN-CLI-018: Ask the factory a question

**Preconditions:** JN-CLI-017. Claude Code is installed and logged in on the
machine running the dashboard.

**Steps:**

1. In "Ask the factory", type `Why did #12 fail?` and send it, or click one of
   the suggested questions.
2. Ask a follow-up, such as `What should I do about it?`.
3. Ask it to requeue the job.
4. Select a job and click "Ask about this job".

**Expected result:**

- Step 1: the question appears, then `Reading the factory state…`, then the
  answer, which names jobs by repository and issue number and uses what the
  page shows: the state, its detail, and how long it has been in it.
- Step 2: the answer takes the earlier turns into account.
- Step 3: the answer says it cannot change anything and says what the steward
  could do instead, such as re-adding the ready label.
- Step 4: it asks `What is happening with <repo> #<issue>?`.
- The header of the panel shows the model, `sonnet` by default (`--model`).

**Notes:**

- Each question runs `claude --print --model <model> --tools ""
  --strict-mcp-config --disable-slash-commands --no-session-persistence` with
  a fixed system prompt, in an empty temporary directory, with the prompt on
  stdin. The session has no tools and no MCP servers, so it cannot run
  commands or read files; the factory state reaches it as JSON in the prompt,
  marked as data.
- The prompt holds the dashboard's current view, the last 10 turns of the
  conversation, and the question. The page keeps the conversation; reloading
  it starts a new one.
- Covered by `TestJourneyDashboardShowsTheStewardsJobsAndAnswers`
  (`internal/e2e`), `TestPrompt`, `TestClaudeArgsGiveTheSessionNoTools`, and
  `TestClaudeAsk` (`internal/dashboard`). Whether the real model answers well
  is not tested.

---

### JN-CLI-019: The dashboard or its ask panel fails

**Preconditions:** None.

**Steps:**

1. Run `schmeide dashboard` with no steward, then with `--interval 0s`.
2. Run it with `--nats-url` pointing at nothing.
3. Run a second dashboard on the same `--addr`.
4. Stop the control node while the dashboard runs, then start it again.
5. Ask a question with Claude Code logged out, or with `--claude` naming a
   missing file.
6. Send a second question before the first is answered, from another tab.
7. Open the page as `http://my-host.example:7878` through a name that points
   at 127.0.0.1.
8. Run it with `--addr 0.0.0.0:7878`.

**Expected result:**

- Step 1: exit code 2, "schmeide dashboard: missing --steward: pass your
  GitHub login with --steward or set SCHMEIDE_STEWARD", or "schmeide
  dashboard: --interval 0s is not positive; pass a duration such as 5s", then
  "Run 'schmeide dashboard --help' for usage.".
- Step 2: exit code 1, "schmeide dashboard: cannot reach the control node's
  NATS at <url> as <steward>: ..." and "Check that `schmeide control` is
  running, that --nats-url points at its --nats-addr, and that --nats-token is
  the token its --runner-tokens lists for <steward>".
- Step 3: exit code 1, "schmeide dashboard: listen on <addr>: ...; pass a
  free address with --addr".
- Step 4: the dashboard logs `status query failed; the page shows the last good
  state` at each poll, and the page shows `control node unreachable`, with the
  error as its tooltip, over the last state it had. Once the control node is
  back, the next poll shows `control node connected` again.
- Step 5: the panel shows the error in place of an answer, for example
  ``claude failed (exit status 1): <its output>; run `<path> auth status` to
  check it is logged in`` or ``"claude" was not found; install Claude Code
  (https://claude.com/claude-code) and log in, or pass --claude with its
  path``. The rest of the page keeps working. A claude that takes longer than
  2 minutes is killed with "claude did not answer within 2m0s; ask a narrower
  question".
- Step 6: the second question gets "another question is still being
  answered; wait for it and ask again".
- Step 7: 403, "the dashboard only answers on localhost; open it as
  http://localhost or http://127.0.0.1".
- Step 8: the dashboard starts and logs a warning, "the dashboard is reachable
  beyond this machine and has no login; anyone who can reach it sees your jobs
  and can ask questions on your claude account", and serves any host name.

**Notes:**

- An empty question gets 400 "the question is empty; type a question and send
  it again", and one over 2,000 bytes gets 400 "the question is longer than
  2000 bytes; shorten it and send it again".
- `POST /api/ask` takes only `application/json` (415 otherwise), so a page on
  another site cannot post a question without a CORS preflight.
- Covered by `TestRun` (`cmd/schmeide`), `TestRunFailsWithoutAControlNode`,
  `TestRunFailsWhenTheAddressIsTaken`,
  `TestBoardFailedQueryKeepsTheLastGoodState`, `TestClaudeAsk`,
  `TestAskerAnswersOneQuestionAtATime`, and `TestHandler`
  (`internal/dashboard`). Steps 4 and 8 have no automated test.
