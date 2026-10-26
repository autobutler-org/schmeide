# `AGENTS.md`

## Purpose

These instructions tell coding agents how to work in this repository. [`CLAUDE.md`](CLAUDE.md) imports this
file, so this is the one place to edit. Never write a second copy of a rule somewhere else.

schmeide is the autobutler-org software factory. A control node (`schmeide control`) runs an embedded NATS
server with JetStream as the job queue and serves the schmeide GitHub App's webhook: an issue that is assigned
and carries the ready label becomes a job on its steward's subject, and the control node mints a per-job GitHub
App token. A runner (`schmeide runner`) connects out to the control node, takes its steward's jobs, and for each
one drives Claude Code or the Grok CLI in a git worktree, runs the target repo's gate, opens a pull request, runs
AI review rounds, and merges only when the repo config sets `automate.merge = "on"`. Everything is one Go binary.

### Repo map

```text
cmd/schmeide/             entrypoint: dispatches control, runner, doctor, status, init, completion, version
cmd/fakeagent/            test stand-in for the claude and grok CLIs, scripted by FAKEAGENT_SCRIPT
internal/control/         control node: webhook intake, embedded NATS JetStream queue, per-job tokens, status
internal/runner/          runner: takes jobs; worktree, harness session, gate, PR, review, fix rounds, merge, stacks
internal/harness/         the Harness contract and its Claude Code, Grok CLI, and Fake implementations
internal/forge/           the code host a job talks to (PRs, comments, checks, merge, merge queue, branch
                          protection); GitHub over HTTP, Fake in memory
internal/repoconfig/      loads and validates a target repo's .schmeide/config.toml
internal/doctor/          `schmeide doctor`: checks a machine can run a runner and says how to fix what fails
internal/ciwatch/         decides when a failed check needs its log for a fix round, and caps that log
internal/lintgate/        detects a repo's own gate (a Makefile `check` target, a pre-commit config)
internal/repomap/         builds a capped map of tracked files and Go symbols for the harness prompt
internal/refguard/        keeps a job worktree from moving other jobs' branches
internal/secretscan/      finds credential-shaped literals in tracked files (schmeide:allow-secret skips a line)
internal/trust/           records which work directory and origin pairs the operator agreed a runner may work
internal/release/         the version stamped into the binary with -ldflags
internal/testutil/fakegh/ stateful fake of the GitHub REST endpoints the runner calls for one repository
internal/e2e/             end-to-end journeys against the built binary (build tag e2e)
docs/                     operator and contributor documentation; architecture/ and user-journeys/ live here
build/                    build output (./build/schmeide, coverage reports); not source
.github/workflows/        ci.yml (lint, build, race tests and coverage gate, govulncheck, e2e, scripts), image.yml, release.yml
```

### Where to read next

- [`docs/QUICKSTART.md`](docs/QUICKSTART.md): from zero to one pull request.
- [`docs/design.md`](docs/design.md): the decisions the team has settled, and the open questions. It records
  decisions, not the current code layout.
- [`ARCHITECTURE.md`](ARCHITECTURE.md) and [`docs/architecture/`](docs/architecture/README.md): how schmeide is
  built, including packages, streams and subjects, job flows, data, and credentials.
- [`docs/user-journeys/`](docs/user-journeys/README.md): stable `JN-<AREA>-NNN` journeys. This is the feature
  inventory; check it before claiming schmeide does something.
- [`docs/config.md`](docs/config.md): the repo config keys and the control and runner settings.
- [`docs/FAILURE-MODES.md`](docs/FAILURE-MODES.md): what happens when a job, runner, or control node fails.
- [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md): running a control node.
- [`docs/GLOSSARY.md`](docs/GLOSSARY.md): the terms. Use them as defined.
- [`docs/permissions.md`](docs/permissions.md): the permission floor every harness session starts from, what the runner refuses, and how a repo opts in to more.
- [`docs/VERSIONING.md`](docs/VERSIONING.md): the version stamp and the control and runner skew warning.

## Working in this repository

### Use Makefile targets (always)

Use the existing targets rather than crafting your own shell commands. If an action needs to be repeatable, add
a target. `make help` lists everything; these are the ones that matter day to day.

| Target          | What it does                                                                                   |
| --------------- | ---------------------------------------------------------------------------------------------- |
| `make build`    | builds `./build/schmeide` with the version from `git describe` stamped in                       |
| `make test`     | `go test ./...`; skips `internal/e2e`                                                           |
| `make test/e2e` | `go test -tags e2e -count=1 ./internal/e2e`; its `TestMain` builds `schmeide` and `fakeagent`   |
| `make check`    | `check/format`, `check/tidy`, `check/vet`, `check/lint`, then `test`; changes no files           |
| `make format`   | `golangci-lint fmt` (gci, gofumpt, goimports); rewrites files                                   |
| `make tidy`     | `go mod tidy`                                                                                   |
| `make install`  | installs the stamped binary into `GOBIN` (or `GOPATH/bin`)                                       |
| `make bench`    | the benchmarks in `internal/control` (webhook intake and the status store)                      |

- The Go toolchain is pinned twice: the `go` line in `go.mod` and `GOTOOLCHAIN` at the top of the `Makefile`.
  Change both together.
- Dev tools (golangci-lint, govulncheck, gofumpt, gci, goimports, air, dlv) are `tool` directives in `go.mod`
  and run as `go tool <name>`. Do not install them globally or add a second pin.
- `.golangci.yml` enables only linters that have an autofix. `make check/lint` reports and does not fix; apply
  the fixes with `go tool golangci-lint run --fix --config .golangci.yml ./...` and commit what it changes.
- CI's `ci.yml` runs `make check/format check/tidy check/vet check/lint`, which fail on any change `make format`
  or `make tidy` would make, and runs the build, the race tests, govulncheck, e2e, and script tests as parallel jobs. Run `make format`,
  `make tidy`, and `make check` before pushing, and commit whatever the first two changed.
- `go test ./...` skips `internal/e2e` because of its `e2e` build tag. Run `make test/e2e` when a change touches
  a flow those journeys cover (intake, job delivery, the gate, opening a pull request, redelivery).
- Test doubles: `cmd/fakeagent` stands in for the claude and grok CLIs so the real harness process code runs;
  `harness.Fake` replaces a harness without a process; `internal/testutil/fakegh` fakes the GitHub REST API over
  HTTP; `forge.Fake` replaces the forge in memory. Extend these rather than writing a new double.

### Documentation (always)

The repository keeps two documentation sets current alongside the code. A pull request that changes behavior or
structure is not done until both are updated in that same pull request, not in a follow-up.

- **`ARCHITECTURE.md` and `docs/architecture/` describe structure.** A new or changed package, JetStream
  stream, subject, consumer, KV bucket or key, persistent file, CLI command, flag that changes a flow, or job
  stage updates the page that owns it, that page's diagram, and the index table in
  `docs/architecture/README.md`. Adding a page also adds a row to the "Read next" table in `ARCHITECTURE.md`.
  Diagrams are Mermaid. When a diagram and the code disagree, the code wins: fix the diagram.
- **`docs/user-journeys/` is the feature inventory and the manual test script.** Any change a steward, operator,
  or repo maintainer could observe adds or updates a journey: a new comment on an issue, a changed error
  string, a new flag or config key, a new failure path, a changed default.
  - Journeys use the fixed format: Preconditions, Steps, Expected result, Notes.
  - IDs (`JN-<AREA>-NNN`) are stable. Never renumber. A new journey takes the next free number in its area.
  - When a behavior is removed, delete its journey. Do not leave it stale.
  - The Notes line names the test that covers the journey (package and test function), or says there is none.
  - Before claiming schmeide does something, check the journeys. Before writing a new e2e test, write or update
    the journey it proves.
- **Be detailed, not brief.** These pages are what the next engineer or agent reads instead of the code.
  - Name the package and file that implement each thing.
  - Cover the edge cases and failure paths, not only the happy path: what is retried, what comments on the
    issue, what leaves the pull request open, what is only logged.
  - Quote user-visible strings (issue comments, CLI errors, log lines an operator searches for) exactly as the
    code emits them, and update the quote when the string changes.
- **Keep the other docs current too.** `README.md`, `docs/config.md`, `docs/FAILURE-MODES.md`, and
  `docs/GLOSSARY.md` change in the same pull request when they describe the thing being changed. A new key or
  flag goes in `docs/config.md`; a new failure path goes in `docs/FAILURE-MODES.md`; a new term goes in the
  glossary.
- **Terminology.** The job owner is the "steward" (the issue assignee), never "submitter". Use the glossary's
  terms, and add a term to the glossary before using it.
- **Style.** American spelling. Plain, factual sentences. No filler.

### Scope discipline

- Advice is not a request for edits. When asked to review, explain, or plan, answer and touch no files.
- Build what was asked. No flags, config keys, or abstractions nobody requested.

## Go conventions

### Key rule (always)

- Respect the linting and formatting conventions of `.golangci.yml` and the formatters it enables. Avoid
  `//nolint`; where one is unavoidable, `nolintlint` requires it to name the linter and give a reason.

### Business logic stays out of the edge

The HTTP handler and each command's `Main` only extract input, call a function, and shape the output.

- `control.Handler` reads the method, headers, and body, calls `Intake.Handle`, and maps its error to a status
  code. Signature checks, allowlists, and job creation live in `Intake` and functions such as
  `JobFromIssuesEvent` and `VerifySignature`, which take plain values and are tested without HTTP.
- Each command's `Main(args, stdout, stderr) int` parses flags with the standard `flag` package (there is no
  cobra), calls a `Run(ctx, cfg, log)` style function, and turns the error into a message and an exit code:
  `0` success, `2` usage error, `1` runtime failure.
- CLI error text says what to do next, as the existing `Run 'schmeide <command> --help' for usage.` lines do.

### Code

- Every package has a package doc comment that says what it does and what it does not do. A new package ships
  with one, and when a package's behavior changes, its doc changes in the same pull request. No linter
  enforces this; review does.
- Wrap errors with context: `fmt.Errorf("...: %w", err)`. Sentinel errors are package-level
  `var Err... = errors.New(...)` and are matched with `errors.Is` (`errorlint` checks this).
- Prefer immutable: `const` where the value never changes, and values passed down in structs rather than shared
  state. Existing package-level `var`s are lookup tables, sentinel errors, compile-time interface checks
  (`var _ Forge = GitHub{}`), a few values tests tune (`pollWait`, `logReadLimit`, `jobStreamMaxMsgs`), and one
  registry (`openWorkspaces` in `internal/runner/cache.go`). Do not add more mutable package-level state.
- Logs use `log/slog` with key-value pairs.
- Every harness session and gate run has a timeout and is canceled through its context, as in
  `internal/runner`. Keep that true for any new subprocess.

### Tests

- Tests sit beside the code they cover. They use the standard `testing` package only; there is no testify.
- Use table-driven tests with `t.Run` where a function has several input cases, as `internal/repoconfig`,
  `internal/control`, and `internal/secretscan` do.
- Use the doubles listed above (`cmd/fakeagent`, `harness.Fake`, `internal/testutil/fakegh`, `forge.Fake`).
  Tests never call GitHub or a real agent CLI.
- End-to-end tests live in `internal/e2e`, one `TestJourney...` function per journey, and run the built binary
  as subprocesses.

## Test-driven development (strongly preferred)

1. Write or update the test that pins the behavior first.
2. Run it and watch it fail (red), which proves the test captures the requirement.
3. Write the least code that makes it pass (green).
4. Refactor with the tests still passing.

A behavior that spans control, runner, and GitHub gets a journey in `docs/user-journeys/` first, then an e2e test
in `internal/e2e` that proves it. A pure refactor runs the existing tests before and after. Cover edge cases,
error paths, and boundary values.

## Backend assumptions

- Never start, stop, or restart a control node or runner yourself, and do not run `make run` or `make watch`.
- Tests are fair game, including `make test/e2e`, which starts its own control and runner subprocesses against
  fakes.

## Pull request and commit conventions (always follow this)

- **Title:** lowercase, imperative mood, no `type:` prefix. GitHub appends the PR number on merge:
  `serve the control node's NATS over TLS (#202)`, `add per-repo automation switches to the repo config (#198)`.
- **Body:** fill the [PR template](.github/pull_request_template.md): Summary, Why do we need this?, How was this
  tested?. Include `Closes #N`. Say which `docs/architecture/` page and which `docs/user-journeys/` journey
  changed, if any.
- **Branch:** `<issue number>_short-description`, for example `206_architecture-and-journey-docs`. The runner
  relies on this prefix to find a stack's base.
- **Commits:** signed off (`git commit -s`).
- **No AI attribution and no session links.** No AI co-author trailer, no AI-generated footer, and no Claude
  Code session link in a commit, PR title, PR body, PR comment, or issue.
- **How was this tested?** lists the commands actually run. Never write "tested manually" unless it happened.
