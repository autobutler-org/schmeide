# schmeide user journeys

This directory documents what schmeide does, from the point of view of the
people who use it: the operator who deploys a control node, the steward who
runs a runner and assigns issues to themselves, and the repo maintainer who
writes `.schmeide/config.toml`. GitHub is the user interface for most of it,
plus the `schmeide` CLI.

The journeys serve three purposes:

1. **Manual test script.** Each journey is a set of steps a person can follow
   with GitHub and a terminal.
2. **Feature inventory.** This directory is the list of what schmeide is
   supposed to do. Before claiming schmeide does something, check here.
3. **Ground truth for automated tests.** The end-to-end tests in
   [`internal/e2e`](../../internal/e2e) (`TestJourney...`) and the unit tests
   across `internal/` check these journeys. Each journey's Notes name the test
   that covers it, or say "No automated test".

A behavior change in code updates or adds a journey in the same pull request.

## Format

Each file covers one feature area. Journeys in a file use this format:

```
### JN-<AREA>-NNN: Journey title

**Preconditions:** What must be true before starting.
**Steps:** Numbered steps the user takes.
**Expected result:** What success looks like (observable: a comment on the issue, a PR opened, a log line, a CLI exit code and message, a label).
**Notes:** Edge cases, which test covers it, related journeys.
```

Journey IDs are stable. Do not renumber when adding, moving, or removing
journeys. A new journey takes the next free number in its file. A removed
journey's number is not reused.

## Files

| File | Feature area |
| --- | --- |
| [setup.md](setup.md) (`JN-SETUP`) | GitHub App, `schmeide init`, starting the control node, `schmeide doctor`, starting a runner, the trust prompt, shell completion |
| [repo-config.md](repo-config.md) (`JN-CFG`) | `.schmeide/config.toml`: the gate, timeouts, `check_wait`, `[automate]`, config errors |
| [issue-to-pr.md](issue-to-pr.md) (`JN-PR`) | Assign and label an issue, webhook intake, the branch, the brief, the gate, opening the pull request, stacked pull requests |
| [review.md](review.md) (`JN-REV`) | Reviewer rounds after the pull request opens, review fix rounds |
| [merge.md](merge.md) (`JN-MERGE`) | Factory merge: check wait, merge queue, CI fix round, branch protection, stacks |
| [failures.md](failures.md) (`JN-FAIL`) | Retries, timeouts, secret scan, token revocation, heartbeats, redelivery, stuck jobs, branch guards, a runner in a terminal, token refresh |
| [pool.md](pool.md) (`JN-POOL`) | Pool runners and jobs whose steward has no registered runner |
| [cli.md](cli.md) (`JN-CLI`) | `schmeide` commands, help, environment variables, `status`, `version`, `init`, `completion` |
| [operations.md](operations.md) (`JN-OPS`) | Running the control node container, NATS TLS, restarts, version skew, logs |

## Conventions

- **Terms.** See [../GLOSSARY.md](../GLOSSARY.md).
  - **steward**: the person who owns a job. For a job from an issue, the
    steward is the issue assignee. Never "submitter".
  - **control node**: the single `schmeide control` process. It receives
    webhooks, holds the job queue in embedded NATS with JetStream, and mints a
    GitHub installation token per job.
  - **runner**: a `schmeide runner` process on a steward's machine. It takes
    that steward's jobs one at a time.
  - **harness**: the agent CLI a runner drives, Claude Code (`claude-code`) or
    the Grok CLI (`grok`).
  - **gate**: the command in `.schmeide/config.toml` that must pass before a
    pull request is opened.
  - **factory merge**: `[automate] merge = "on"`. The runner merges after the
    review passes and the checks are green.
- **Steps** use present tense: "Assign", "Run", "Open".
- **Expected results** describe what the user can observe: the GitHub UI
  (comments, pull requests, branches, the App's webhook deliveries), CLI
  output, exit codes, and log lines. They do not describe internals.
- **Preconditions** reference other journey IDs for setup, for example
  "JN-SETUP-019 done".
- Quoted strings (comments, errors, log messages) are copied from the code.
  `<url>`, `<n>`, and similar placeholders stand for values that vary.
- Every outcome comment the runner posts on an issue ends with a hidden HTML
  marker, `<!-- schmeide:outcome <state> -->`. It does not show in the GitHub
  UI. The quoted comment texts below leave it out.

For how the parts fit together, see [../architecture/README.md](../architecture/README.md).
For setup from scratch, see [../QUICKSTART.md](../QUICKSTART.md).
