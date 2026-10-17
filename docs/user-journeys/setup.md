# Setup journeys

Covers getting from nothing to a runner that can take a job: the GitHub App,
`schmeide init`, the control node's required settings, `schmeide doctor`, the
runner, the first-job trust prompt, and shell completion. For a walkthrough,
see [../QUICKSTART.md](../QUICKSTART.md). For running the control node in a
container, see [operations.md](operations.md) and
[../DEPLOYMENT.md](../DEPLOYMENT.md).

---

### JN-SETUP-001: Create the GitHub App

**Preconditions:** The operator can create GitHub Apps in the org. A public
HTTPS URL reaches the machine that will run the control node, through a
TLS-terminating proxy.

**Steps:**

1. In the org, open **Settings**, **Developer settings**, **GitHub Apps**,
   **New GitHub App**.
2. Set **Webhook URL** to `https://<your-host>/webhook`.
3. Set **Webhook secret** to a long random string and keep a copy.
4. Under **Repository permissions**, set Contents: read and write, Pull
   requests: read and write, Issues: read and write, Checks: read-only,
   Administration: read-only.
5. Under **Subscribe to events**, check **Issues**.
6. Create the App and note its **App ID**.
7. Generate a private key and save the `.pem` file.
8. Run `chmod 0600 /path/to/app.pem`.

**Expected result:**

- The App exists with an App ID, a webhook secret, and a private key file
  readable by its owner only.

**Notes:**

- schmeide does not create the App. This journey is manual.
- The control node mints each job's token with only contents: write,
  pull_requests: write, issues: write, and checks: read, limited to the job's
  repository (`internal/control/token.go`). See JN-MERGE-011 for what that
  means for the Administration permission.
- A key that group or other can read stops the control node (JN-SETUP-008).
- No automated test. Related: [../permissions.md](../permissions.md),
  [../architecture/credentials.md](../architecture/credentials.md).

---

### JN-SETUP-002: Install the App on the org

**Preconditions:** JN-SETUP-001 done.

**Steps:**

1. Open the App's settings and choose **Install App**.
2. Install it on the org, on the repositories schmeide may work on.
3. Note the installation ID at the end of the installation's settings URL
   (`.../settings/installations/<id>`) if you plan to allow by installation.

**Expected result:**

- The App appears under the org's installed GitHub Apps with access to the
  chosen repositories.

**Notes:**

- The control node accepts jobs only from orgs in `--allowed-orgs` or
  installations in `--allowed-installations` (JN-PR-007).
- No automated test.

---

### JN-SETUP-003: Write starter config files with `schmeide init`

**Preconditions:** A `schmeide` binary is on `PATH`. The current directory has
no `.schmeide/config.toml` and no `.schmeide/control.conf`.

**Steps:**

1. Run `schmeide init`.

**Expected result:**

- Output:

  ```text
  wrote .schmeide/config.toml
  wrote .schmeide/control.conf
  Next: set the gate in .schmeide/config.toml, and replace every replace-me in .schmeide/control.conf.
  ```

- Exit code 0. `.schmeide/config.toml` has `gate = "make check test"`.
  `.schmeide/control.conf` has `replace-me` placeholders for the webhook
  secret, runner tokens, App ID, private key file, and allowed orgs.

**Notes:**

- Both files are written with mode 0600.
- `.schmeide/control.conf` is an operator file. Once filled in it holds
  secrets. Do not commit it to the target repository.
- The refusal to overwrite is JN-CLI-014.
- Implemented in `cmd/schmeide/init.go`. Covered by `TestInitWritesBothFiles`
  and `TestInitConfigParses` (`internal/control/cli_test.go`).

---

### JN-SETUP-004: Start the control node with environment variables

**Preconditions:** JN-SETUP-001 and JN-SETUP-002 done. The proxy forwards
`https://<your-host>/webhook` to port 8080 on this machine.

**Steps:**

1. Set the required settings in the environment:

   ```sh
   export SCHMEIDE_WEBHOOK_SECRET=...
   export SCHMEIDE_GITHUB_APP_ID=...
   export SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE=/path/to/app.pem
   export SCHMEIDE_RUNNER_TOKENS='octocat:a-token-for-octocat'
   export SCHMEIDE_ALLOWED_ORGS=your-org
   ```

2. Run `schmeide control`.

**Expected result:**

- The process keeps running and logs to stderr:

  ```text
  level=INFO msg="control node started" webhook=http://[::]:8080/webhook listener="plain HTTP; must sit behind a TLS-terminating proxy" nats=127.0.0.1:4222 nats_tls=false ready_label=schmeide:ready
  ```

- A `schmeide-data` directory appears in the working directory (the default
  `--data-dir`).
- In the App's **Advanced** tab, a redelivered ping or issue event gets a 2xx
  response.

**Notes:**

- `--runner-tokens` is comma-separated `<steward login>:<token>` entries, one
  per steward. A token entry that is not `<login>:<token>` fails with "a runner
  token entry is not '<steward login>:<token>'". A steward listed twice fails
  with "steward \"x\" has more than one runner token".
- Environment variables keep secrets out of the process list.
- Implemented in `internal/control/cli.go` and `internal/control/control.go`.
  Covered by `TestParseControl` and `TestRunLogsPlainHTTPAndShutsDown`.

---

### JN-SETUP-005: Start the control node from a config file

**Preconditions:** JN-SETUP-003 done, and every `replace-me` in
`.schmeide/control.conf` replaced.

**Steps:**

1. Run `schmeide control --config .schmeide/control.conf`.
2. Stop it. Run `SCHMEIDE_HTTP_ADDR=:9090 schmeide control --config .schmeide/control.conf`.
3. Stop it. Run `SCHMEIDE_HTTP_ADDR=:9090 schmeide control --config .schmeide/control.conf --http-addr :9191`.

**Expected result:**

- Step 1 starts with the file's settings (JN-SETUP-004's log line).
- Step 2 logs `webhook=http://[::]:9090/webhook`: the environment wins over the
  file.
- Step 3 logs `webhook=http://[::]:9191/webhook`: the flag wins over the
  environment.

**Notes:**

- The file has one `name = value` line per setting, named as the flags are.
  Blank lines and lines starting with `#` are skipped. Surrounding double
  quotes on a value are removed.
- `SCHMEIDE_CONFIG` names the file instead of `--config`.
- A line without `=` fails: "`<file>:<line>: expected 'name = value', got "..."`".
  An unknown name fails: "config file `<file>`: unknown setting "x"; 'schmeide
  control --help' lists the settings". A missing file fails: "read config
  file: ...; check the path given with --config or SCHMEIDE_CONFIG". All exit 2.
- An empty environment variable does not count as set.
- Precedence is also JN-CLI-005. Covered by `TestParseControl`.

---

### JN-SETUP-006: Control node refuses to start without required settings

**Preconditions:** None of the required settings is set.

**Steps:**

1. Run `schmeide control`.

**Expected result:**

- Exit code 2. Stderr:

  ```text
  schmeide control: missing required settings:
    runner-tokens: pass --runner-tokens, set SCHMEIDE_RUNNER_TOKENS, or add 'runner-tokens = ...' to the --config file
    webhook-secret: pass --webhook-secret, set SCHMEIDE_WEBHOOK_SECRET, or add 'webhook-secret = ...' to the --config file
    github-app-id: pass --github-app-id, set SCHMEIDE_GITHUB_APP_ID, or add 'github-app-id = ...' to the --config file
    github-app-private-key-file: pass --github-app-private-key-file, set SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE, or add 'github-app-private-key-file = ...' to the --config file
  Run 'schmeide control --help' for usage.
  ```

**Notes:**

- Only the settings that are missing are listed.
- Covered by `TestRun` (`cmd/schmeide/main_test.go`) and `TestParseControl`.

---

### JN-SETUP-007: Control node refuses an empty or malformed allowlist

**Preconditions:** Every required setting is set except the allowlist.

**Steps:**

1. Run `schmeide control` with neither `--allowed-orgs` nor
   `--allowed-installations`.
2. Run it with `--allowed-installations my-org`.

**Expected result:**

- Step 1: exit code 2, "no allowed orgs or installations; name the orgs or
  GitHub App installations that may create jobs with --allowed-orgs or
  --allowed-installations; fix --allowed-orgs or --allowed-installations".
- Step 2: exit code 2, "allowed installation "my-org" is not a numeric
  installation ID; find the ID at the end of the installation's settings URL
  (.../settings/installations/<id>); fix --allowed-orgs or
  --allowed-installations".

**Notes:**

- The allowlist cannot be empty, so a control node never accepts every
  installation of the App by default.
- Org logins match case-insensitively.
- Covered by `TestEmptyAllowlistAllowsNoOne` and `TestParseControl`.

---

### JN-SETUP-008: Control node refuses a private key others can read

**Preconditions:** Settings complete. The key file has mode 0644.

**Steps:**

1. Run `schmeide control`.
2. Run `chmod 0600 /path/to/app.pem` and run `schmeide control` again.

**Expected result:**

- Step 1: exit code 2, "schmeide control: GitHub App private key
  /path/to/app.pem has mode 0644, readable by group or other; run 'chmod 0600
  /path/to/app.pem' and start again".
- Step 2 starts.

**Notes:**

- A path that does not exist fails with "GitHub App private key: stat ...;
  fix --github-app-private-key-file", exit 2. That is what an unedited
  `replace-me` from `schmeide init` gives.
- A file that is not a PEM RSA key fails at start with "GitHub App private key
  <path>: no PEM block found" or a parse error, exit 1.
- `schmeide doctor --github-app-private-key-file <path>` runs the same check,
  and `--fix` sets mode 0600 (JN-SETUP-017).
- Covered by `TestControlRefusesReadableKeyFile`.

---

### JN-SETUP-009: Control node refuses a bad NATS or listener setting

**Preconditions:** Settings otherwise complete.

**Steps:**

1. Run `schmeide control --nats-tls-cert-file cert.pem` without a key file.
2. Run it with both files set, where the files are not a matching pair.
3. Run it while another process holds port 4222.
4. Run it while another process holds port 8080.
5. Run it with `--webhook-rate 0` or `--delivery-ttl soon`.

**Expected result:**

- Step 1: exit code 2, "NATS TLS needs both a certificate and a key; set both
  --nats-tls-cert-file and --nats-tls-key-file (SCHMEIDE_NATS_TLS_CERT_FILE and
  SCHMEIDE_NATS_TLS_KEY_FILE), or neither".
- Step 2: exit code 1, "schmeide control: load the NATS TLS certificate and
  key: ...; fix --nats-tls-cert-file and --nats-tls-key-file".
- Step 3: exit code 1, "embedded nats did not start on "127.0.0.1:4222" within
  10s; is the address already in use?".
- Step 4: exit code 1, "listen for webhooks on :8080: ...".
- Step 5: exit code 2, "webhook rate "0" is not a positive number of requests
  per minute; pass --webhook-rate or set SCHMEIDE_WEBHOOK_RATE", or "delivery
  ttl "soon" is not a positive duration (for example 10m); pass --delivery-ttl
  or set SCHMEIDE_DELIVERY_TTL".

**Notes:**

- Covered by `TestParseControlNeedsBothTLSFiles` and
  `TestNATSTLSConfigRejectsBadFiles`. Steps 3 to 5 have no automated test of
  the message.

---

### JN-SETUP-010: `schmeide doctor` passes

**Preconditions:** The control node runs (JN-SETUP-004). On the runner
machine: `git` and `gh` are on `PATH`, `gh auth status` succeeds, Claude Code
or the Grok CLI is installed and logged in, and at least 1 GiB is free on the
disk holding the current directory.

**Steps:**

1. Run `schmeide doctor --steward octocat --nats-token a-token-for-octocat`.

**Expected result:**

- Exit code 0. Output grouped by who fixes each check:

  ```text
  operator:
    ok    harness claude-code: installed and logged in, version <x>
    skip  harness grok: not selected: harness not installed: "grok" was not found; install the Grok CLI (https://x.ai/cli/install.sh) and make sure it is on PATH
    ok    permission floor: a session may edit its worktree and is not handed flags that skip permission checks; ...
    ok    control node: reachable at nats://127.0.0.1:4222
  machine:
    ok    git: /usr/bin/git
    ok    gh: /usr/bin/gh is logged in
    ok    disk: <n> MiB free
    ok    lint gate: no Makefile check target or pre-commit config
  This machine is ready. Start the runner with 'schmeide runner --steward <your GitHub login>'.
  ```

**Notes:**

- One ready harness is enough. Another harness that fails is `skip` with "not
  selected:", not `FAIL`.
- The lint gate line reports a Makefile target named `check` and a
  pre-commit config in the current directory. It never fails.
- The permission floor line never fails ([../permissions.md](../permissions.md)).
- `--json` writes a JSON list of `{name, group, status, detail}` to stdout
  and nothing else; the "ready" line is left out.
- Use the same `--nats-url` the runner will use (`tls://<host>:4222` for a
  remote control node).
- Implemented in `internal/doctor`. Covered by `TestReportAllGood`,
  `TestReportPassesWhenOneHarnessIsReady`, `TestReportGroupsChecks`,
  `TestMainJSON`.

---

### JN-SETUP-011: `schmeide doctor` fails when git is missing

**Preconditions:** `git` is not on `PATH`.

**Steps:**

1. Run `schmeide doctor --steward octocat --nats-token <token>`.

**Expected result:**

- The machine group shows `FAIL  git: git was not found on PATH; install it
  (https://git-scm.com/downloads) and try again`.
- Stderr ends with "schmeide doctor: fix the failed checks above, then run
  'schmeide doctor' again." Exit code 1.

**Notes:**

- Git fails doctor even when a harness is ready.
- Covered by `TestReportMissingGit` and `TestReportGitFailsBesideAReadyHarness`.

---

### JN-SETUP-012: `schmeide doctor` fails when gh is missing or logged out

**Preconditions:** `gh` is not on `PATH`, or `gh auth status` fails.

**Steps:**

1. Run `schmeide doctor --steward octocat --nats-token <token>`.

**Expected result:**

- Missing: `FAIL  gh: gh was not found on PATH; install the GitHub CLI
  (https://cli.github.com) and try again`.
- Logged out: `FAIL  gh: gh is not logged in (<first line of gh's output>);
  run 'gh auth login' and try again`.
- Exit code 1.

**Notes:**

- The runner itself does not check `gh` on start. Doctor does.
- Covered by `TestGHCheck`.

---

### JN-SETUP-013: `schmeide doctor` fails when the disk is full

**Preconditions:** Less than 1 GiB is free on the disk holding the current
directory.

**Steps:**

1. Run `schmeide doctor --steward octocat --nats-token <token>`.

**Expected result:**

- `FAIL  disk: the disk holding . is full: <n> MiB free, want at least 1024
  MiB; free some space and try again`. Exit code 1.

**Notes:**

- Doctor checks the current directory's disk, not `--work-dir`.
- Covered by `TestDiskCheck`.

---

### JN-SETUP-014: `schmeide doctor` fails when no harness is ready

**Preconditions:** Neither Claude Code nor the Grok CLI is installed and
logged in.

**Steps:**

1. Run `schmeide doctor --steward octocat --nats-token <token>`.

**Expected result:**

- Each harness is `FAIL` with its fix, for example:
  - `FAIL  harness claude-code: harness not installed: "claude" was not found;
    install Claude Code (https://claude.com/claude-code) and make sure it is on
    PATH`
  - ``FAIL  harness claude-code: harness not logged in: run `claude auth login` on this machine, then try again``
  - ``FAIL  harness grok: harness not logged in: run `grok login` on this machine, then try again``
- Exit code 1.

**Notes:**

- Claude Code is checked with `claude auth status --json`, Grok with `grok
  models`. An unreadable answer says to run the command by hand and upgrade
  the CLI.
- Covered by `TestReportHarnessNotInstalled`, `TestReportFailsWhenEveryHarnessFails`,
  `TestMainFailsWithoutAHarness`.

---

### JN-SETUP-015: `schmeide doctor` fails when the control node cannot be reached

**Preconditions:** The control node is not running, `--nats-url` is wrong, or
`--nats-token` does not match the steward's entry in `--runner-tokens`.

**Steps:**

1. Run `schmeide doctor --steward octocat --nats-token <token>`.

**Expected result:**

- `FAIL  control node: cannot reach the control node's NATS at <url> as
  octocat: <error>` followed by "Check that `schmeide control` is running,
  that --nats-url points at its --nats-addr, and that --nats-token is the
  token its --runner-tokens lists for octocat". Exit code 1.
- When something answers on the URL but it is not a control node: "connected
  to NATS at <url> but no control node answered for octocat: ..." followed by
  "Check that --nats-url points at a `schmeide control` node and not at some
  other NATS server".

**Notes:**

- A wrong token shows as the "cannot reach" message, with an authorization
  error in `<error>`.
- Covered by `TestReportSaysHowToFixEachFailure`, `TestReportWrongRunnerToken`,
  `TestReportControlNodeFailsBesideAReadyHarness`.

---

### JN-SETUP-016: `schmeide doctor` without a steward

**Preconditions:** `SCHMEIDE_STEWARD` is unset.

**Steps:**

1. Run `schmeide doctor`.

**Expected result:**

- `FAIL  control node: no steward to connect as; pass your GitHub login with
  --steward`. Exit code 1.

**Notes:**

- Covered by `TestReportEmptySteward`.

---

### JN-SETUP-017: Check and fix the private key mode with doctor

**Preconditions:** On the control node machine. The App key file has mode
0644.

**Steps:**

1. Run `schmeide doctor --github-app-private-key-file /path/to/app.pem`.
2. Run `schmeide doctor --github-app-private-key-file /path/to/app.pem --fix`.

**Expected result:**

- Step 1: `FAIL  github app private key: GitHub App private key
  /path/to/app.pem has mode 0644, readable by group or other; run 'chmod 0600
  /path/to/app.pem' and start again`. Exit code 1 (other checks may also fail
  on a machine that is not a runner).
- Step 2: the key's mode becomes 0600, the check shows `ok ... is readable by
  its owner only`, and the output includes "--fix: fixed github app private
  key.".

**Notes:**

- Without `--github-app-private-key-file` the check is not run.
- `--fix` with nothing fixable prints "--fix: nothing was changed; no failed
  check can be fixed safely.".
- Covered by `TestKeyFileCheck` and `TestMainFix`.

---

### JN-SETUP-018: Pick a harness

**Preconditions:** Both Claude Code and the Grok CLI are installed and logged
in.

**Steps:**

1. Start a runner (JN-SETUP-019).
2. Run `schmeide status --steward <login> --nats-token <token>`.

**Expected result:**

- The runner's row lists `claude-code,grok` under HARNESSES.
- Claude Code writes the change. The Grok CLI reviews it (JN-REV-001).

**Notes:**

- The writer is the first ready harness in the order Claude Code, then Grok.
  Grok writes only when Claude Code is not usable.
- Covered by `TestAllOrdersClaudeCodeBeforeGrok`,
  `TestGrokSelectedOnlyWhenClaudeCodeIsNotUsable`,
  `TestReviewRunsOnTheOtherReadyHarness`.

---

### JN-SETUP-019: Start a runner

**Preconditions:** JN-SETUP-010 passes on this machine.

**Steps:**

1. Run `SCHMEIDE_NATS_TOKEN=a-token-for-octocat schmeide runner --steward octocat`.
   For a remote control node add `--nats-url tls://<host>:4222`.

**Expected result:**

- The runner keeps running and logs `level=INFO msg="runner registered"
  steward=octocat harnesses=claude-code pool=false nats=<url>`.
- The control node logs `level=INFO msg="runner registered" steward=octocat
  host=<hostname> harnesses=claude-code pool=false version=<version>`.
- `schmeide status` lists the runner (JN-CLI-007).

**Notes:**

- The runner creates `schmeide-work/` (the default `--work-dir`) for clones
  and worktrees.
- The registration does not expire. A stopped runner stays listed, and its
  steward keeps their jobs (JN-POOL-003).
- Covered by `TestRunnerOnlyTakesItsStewardsJobs` and
  `TestTrackerRecordsRegisteredRunners`.

---

### JN-SETUP-020: Runner refuses to start

**Preconditions:** None.

**Steps:**

1. Run `schmeide runner` with no steward.
2. Run `schmeide runner --steward octocat --claude-args "--dangerously-skip-permissions"`.
3. Run `schmeide runner --steward octocat` with no control node at `--nats-url`.
4. Run it on a machine with no ready harness.
5. Run it on a machine without git.

**Expected result:**

- Step 1: exit code 2, "schmeide runner: missing --steward: pass your GitHub
  login with --steward or set SCHMEIDE_STEWARD" and "Run 'schmeide runner
  --help' for usage.".
- Step 2: exit code 2, "--claude-args contains "--dangerously-skip-permissions",
  which lets a session skip its permission checks; remove it, or pass
  --allow-dangerous-harness-args to accept that (see docs/permissions.md)".
- Step 3: exit code 1, the same "cannot reach the control node's NATS ...
  Check that `schmeide control` is running ..." text as JN-SETUP-015.
- Step 4: exit code 1, "no usable harness on this machine:" with one line per
  harness and its fix, then "Fix one of them, then run `schmeide doctor`".
- Step 5: exit code 1, "git was not found on PATH; install git, then run
  `schmeide doctor`".

**Notes:**

- A `--permission-mode` other than `acceptEdits` in `--claude-args` or
  `--grok-args` is refused the same way.
- A `SCHMEIDE_<NAME>` value that does not parse for its flag, such as
  `SCHMEIDE_ACK_WAIT="5 minutes"`, is refused with exit code 2 before anything
  else is checked (JN-CLI-006).
- A steward with `.`, `*`, `>`, `/`, `\`, or whitespace, or the reserved name
  `_pool`, fails with "steward "x" is not a GitHub login; pass your login with
  --steward", exit 1.
- Covered by `TestMainExitCodes`, `TestMainRefusesWideningHarnessArgs`,
  `TestNewWithoutControlNodeSaysWhatToCheck`,
  `TestNewWithWrongTokenSaysWhatToCheck`,
  `TestNewWithoutUsableHarnessSaysHowToFix`, `TestPoolNameIsNotASteward`.

---

### JN-SETUP-021: Trust a new work directory and origin on the first job

**Preconditions:** A runner started on a terminal (JN-SETUP-019), without
`SCHMEIDE_TRUST`. No trust decision is recorded for this work directory and
repository.

**Steps:**

1. Assign an issue to the steward and add `schmeide:ready` (JN-PR-001).
2. In the runner's terminal, answer `y` to the prompt.

**Expected result:**

- The runner prints on stderr: "Let schmeide run agent sessions and gate
  commands from https://github.com/<owner>/<repo>.git in <absolute work dir>?
  [y/N]".
- After `y`, the job continues to a pull request.
- A file appears under the user config directory in `schmeide/trust/` (for
  example `~/.config/schmeide/trust/` on Linux, `~/Library/Application
  Support/schmeide/trust/` on macOS).
- Later jobs for the same repository and work directory do not ask.

**Notes:**

- `y` and `yes`, in any case, are yes. Anything else is no.
- The prompt comes before the worktree is created.
- Implemented in `internal/trust`. Covered by `TestRecordedYesAllows` and
  `TestConfirmReadsYes`.

---

### JN-SETUP-022: Decline trust at the prompt

**Preconditions:** As JN-SETUP-021.

**Steps:**

1. Assign and label an issue.
2. Answer `n` (or press Enter) at the prompt.

**Expected result:**

- Nothing is recorded. The attempt fails with "work directory <dir> was not
  trusted for <origin>; restart the runner to be asked again, or set
  SCHMEIDE_TRUST=1".
- The job is retried after 30 seconds and the prompt appears again, up to
  `--max-attempts` (default 3). After the last refusal the issue gets the
  comment "schmeide could not finish this job and did not open a pull
  request:" followed by that error.

**Notes:**

- A refusal is a failed work outcome and counts toward `--max-attempts`
  (JN-FAIL-001).
- Covered by `TestNoIsNotRecorded`.

---

### JN-SETUP-023: Trust every pair for one run with `SCHMEIDE_TRUST=1`

**Preconditions:** An unattended runner, for example under a service manager.

**Steps:**

1. Start the runner with `SCHMEIDE_TRUST=1` in its environment.
2. Assign and label an issue.

**Expected result:**

- No prompt. The job runs.
- Nothing is written under `schmeide/trust/`. A later run without the variable
  is asked (or refused) again.

**Notes:**

- Only the exact value `1` counts.
- Covered by `TestEnvAllowsWithoutRecording`.

---

### JN-SETUP-024: Non-interactive runner refuses an untrusted pair

**Preconditions:** A runner whose stdin is not a terminal (for example
`schmeide runner ... < /dev/null`), without `SCHMEIDE_TRUST=1`, and no
recorded yes for the pair.

**Steps:**

1. Assign and label an issue.

**Expected result:**

- No prompt. Each attempt fails with "work directory <dir> has not been
  trusted for <origin>; run the runner on a terminal once to answer, or set
  SCHMEIDE_TRUST=1 to trust it for this run".
- After `--max-attempts` attempts the issue gets "schmeide could not finish
  this job and did not open a pull request:" with that error. No worktree is
  created and no pull request is opened.

**Notes:**

- Covered by `TestFreshPairIsRefused` and `TestWorkRefusesAnUntrustedPair`.

---

### JN-SETUP-025: A different origin asks again

**Preconditions:** JN-SETUP-021 done for repository A.

**Steps:**

1. With the same runner and `--work-dir`, assign and label an issue in
   repository B.

**Expected result:**

- The runner asks again, naming repository B's origin URL.

**Notes:**

- The decision is keyed on the canonical work directory (symlinks resolved)
  and the origin URL. Changing `--work-dir` or `--github-url` also asks again.
- Covered by `TestChangedOriginIsRefused`.

---

### JN-SETUP-026: Install bash completion

**Preconditions:** bash.

**Steps:**

1. Add `source <(schmeide completion bash)` to `~/.bashrc` and open a new
   shell.
2. Type `schmeide ` and press Tab twice.
3. Type `schmeide completion ` and press Tab twice.

**Expected result:**

- Step 2 offers `control runner doctor status init completion version -h --help`.
- Step 3 offers `bash zsh fish`.
- After any other command, Tab offers `-h --help`.

**Notes:**

- Covered by `TestCompletion`, which also runs `bash -n` on the script.

---

### JN-SETUP-027: Install zsh completion

**Preconditions:** zsh with `compinit` loaded.

**Steps:**

1. Add `source <(schmeide completion zsh)` to `~/.zshrc`, after `compinit`,
   and open a new shell.
2. Type `schmeide ` and press Tab.

**Expected result:**

- zsh lists `control`, `runner`, `doctor`, `status`, `init`, `completion`,
  and `version`, each with a description.

**Notes:**

- Covered by `TestCompletion` and `TestCompletionListsEveryCommand`.

---

### JN-SETUP-028: Install fish completion

**Preconditions:** fish.

**Steps:**

1. Run `schmeide completion fish > ~/.config/fish/completions/schmeide.fish`.
2. Open a new shell, type `schmeide ` and press Tab.

**Expected result:**

- fish lists `control`, `runner`, `doctor`, `status`, `init`, `completion`,
  and `version` with descriptions. After `schmeide completion `, it offers
  `bash`, `zsh`, and `fish`.

**Notes:**

- Covered by `TestCompletion` and `TestCompletionListsEveryCommand`.
