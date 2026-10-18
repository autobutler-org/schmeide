# Build and tooling

## Commands

`cmd/schmeide/main.go` is a plain `switch` on the first argument, with no CLI framework. Each subcommand owns
its flags and help text. Running with no arguments or `-h` prints the command list.

| Command | Entry point | Purpose |
| --- | --- | --- |
| `schmeide control` | `control.Main` (`internal/control/cli.go`) | The control node. See [Control node](control-node.md). |
| `schmeide runner` | `runner.Main` (`internal/runner/cli.go`) | A runner. See [Runner](runner.md). |
| `schmeide doctor` | `doctor.Main` (`internal/doctor/doctor.go`) | Checks a runner machine: git, gh, free disk, lint gate detection, each harness, the permission floor, and that a control node answers on NATS. `--fix` and `--json` are supported. |
| `schmeide status` | `control.StatusMain` (`internal/control/status.go`) | Asks the control node on `schmeide.status-query.<steward>` and prints the steward's own live registrations and unfinished jobs, plus pool jobs for a pool steward. `--json` for the stable `Snapshot` form. |
| `schmeide init` | `initMain` (`cmd/schmeide/init.go`) | Writes `.schmeide/config.toml` and `.schmeide/control.conf` (mode 0600) in the current directory, and refuses to overwrite either. |
| `schmeide completion bash\|zsh\|fish` | `completionMain` (`cmd/schmeide/completion.go`) | Prints a completion script. |
| `schmeide version` | `versionMain` (`cmd/schmeide/version.go`) | Prints `release.Version`. |

Runner and control settings share one convention: every flag can come from `SCHMEIDE_<FLAG_NAME>` and a flag
wins over the environment. The control node also reads a `--config` file, which the environment wins over.

`cmd/fakeagent` is a test stand-in for an agent CLI. It ignores its arguments and stdin and runs the script in
`FAKEAGENT_SCRIPT`, one step per line: `edit <path> <text>`, `stderr <text>`, `exit <n>`, `sleep <seconds>`, and
`hang`. Text expands `$NAME` from the environment and `$$` to the process ID. It lets tests drive the real
`harness.ClaudeCode` and `harness.Grok` process code, including process-group kills.

## Makefile targets

`make help` lists every target with a `##` comment. The ones that matter:

| Target | What it runs |
| --- | --- |
| `make build` | `go build -ldflags "-X …/internal/release.Version=$(VERSION)" -o build/schmeide ./cmd/schmeide` |
| `make install` | `go install` with the same ldflags, into `GOBIN` |
| `make test` | `go test ./...` (unit and integration tests, no e2e) |
| `make test/race` | `go test -race -covermode=atomic` over `./...`; `PKGS=` picks packages, `COVERPROFILE=` writes a profile |
| `make test/scripts` | runs each `scripts/*_test.sh` with bash |
| `make test/e2e` | `go test -tags e2e -count=1 ./internal/e2e`, and prints how long it took |
| `make test/cover` | coverage profile and HTML under `build/` |
| `make bench` | `go test -bench . -benchmem ./internal/control` (`BenchmarkWebhookIntake`, `BenchmarkStatusStore`) |
| `make check` | `check/format` (gofmt -l), `check/tidy` (`go mod tidy -diff`), `check/vet`, `check/lint` (golangci-lint without fixing), then `test` and `test/scripts`. Changes no files. |
| `make check/vuln` | `go tool govulncheck ./...` (needs network, so not part of `check`; the `govulncheck` CI job runs it) |
| `make format` | `go tool golangci-lint fmt` (gci, gofumpt, goimports) |
| `make changelog` | `CHANGELOG.md` from merged PRs, needs `gh` logged in |
| `make watch` | rebuild on change with `air` |

The Makefile exports `GOTOOLCHAIN=go1.26.9+auto`, and `go.mod` pins `go 1.26.9`. Linters and formatters are Go
tool dependencies in `go.mod` (`tool` block: golangci-lint, gci, gofumpt, goimports, govulncheck, staticcheck,
air, dlv), run with `go tool`, so no separate install is needed. `VERSION` defaults to
`git describe --tags --always --dirty`.

## Lint configuration

`.golangci.yml` (golangci-lint v2) enables only linters that can autofix, among them `errorlint`, `gocritic`,
`godot`, `misspell`, `perfsprint`, `revive`, `staticcheck`, `usestdlibvars`, and `whitespace`, plus the `gci`,
`gofumpt`, and `goimports` formatters. `nolintlint` requires every `//nolint` to name the linter and give a
reason. `nakedret` allows naked returns only in functions up to 10 lines.

What that implies: `make check/lint` fails on anything these linters report, but most reports can be fixed
mechanically. Linters without autofix (for example `errcheck`, `gosec`, `unused`) are not enabled, so the code
review and tests carry those concerns. Comments end in a period (`godot`).

## CI workflows

Three workflows are in `.github/workflows/`. `ci.yml` and `image.yml` run on pull requests to `main` and on
`merge_group`, so they work with a merge queue; `ci.yml` also runs on pushes to `main` and on manual dispatch.
A newer push to a PR branch cancels the older `ci.yml` run, and pushes to `main` always run to completion
because each one records a coverage baseline.

| Workflow | Triggers | Does | Produces |
| --- | --- | --- | --- |
| `ci.yml` (ci) | PR to `main`, merge group, push to `main`, manual | the five parallel jobs below | pass or fail per job |
| `image.yml` (Image) | PR to `main`, merge group, manual | Buildx build of the `Dockerfile` for linux/amd64 and linux/arm64, not pushed | proof the Dockerfile builds |
| `release.yml` (Release) | push of a `v*` tag | Buildx build for both platforms with `VERSION=<tag>`, push to GHCR | `ghcr.io/autobutler-org/schmeide:<version without v>` and `latest` |

Jobs of `ci.yml`:

| Job | Runs | Notes |
| --- | --- | --- |
| `lint` | `make check/format check/tidy check/vet check/lint`, then `make build` | These fail on any change `make format` or `make tidy` would make. 15 minute timeout. |
| `test` | `make test/race COVERPROFILE=cover.out`, then `scripts/cover-report.sh cover.out` | The whole suite under the race detector in one job. Writes the slowest packages and the coverage report to the job summary. A PR fails when its total coverage drops more than one point below the last `main` push's total; if no baseline exists (first run, expired artifact) the gate is skipped. Pushes to `main` upload the total as the `coverage-baseline` artifact. 20 minute timeout. |
| `govulncheck` | `make check/vuln` | Uses the latest patched Go (`go-version: stable`) rather than `go.mod`'s, so standard library fixes count. |
| `e2e` | `make test/e2e` | The journeys in [End-to-end journeys](#end-to-end-journeys). |
| `scripts` | `make test/scripts` | Runs every `scripts/*_test.sh`. |

`make check` (local) runs `check/format`, `check/tidy`, `check/vet`, `check/lint`, `test`, and `test/scripts`.
It does not run the race tests, `check/vuln`, or e2e; those are CI jobs, so run `make test/race` or
`make test/e2e` yourself when a change needs them.

## Version stamp and the image

`internal/release.Version` is `dev` unless the linker sets it. `make build`, `make install`, and the
`Dockerfile` (build arg `VERSION`) set it with `-X github.com/autobutler-org/schmeide/internal/release.Version=…`.
`release.Skewed` compares two versions only when both are valid semver. The control node uses it to warn about a
runner on another version. Details are in [VERSIONING.md](../VERSIONING.md).

The `Dockerfile` cross-compiles with `CGO_ENABLED=0` on the build platform (`golang:1.26.9`), and the final stage
is `gcr.io/distroless/static:nonroot`, UID 65532, with `/data` as a volume, `SCHMEIDE_DATA_DIR=/data`, ports 8080
and 4222, entrypoint `schmeide`, and default command `control`. It has no shell, git, or harness, so it cannot run
a runner. Running it is in [DEPLOYMENT.md](../DEPLOYMENT.md).

## Test fakes

| Fake | Package | Stands in for |
| --- | --- | --- |
| `harness.Fake` | `internal/harness/fake.go` | a harness, in process. `RunFunc` scripts each session. |
| `forge.Fake` | `internal/forge/fake.go` | the `Forge` interface, in memory |
| `fakegh.Server` | `internal/testutil/fakegh` | the GitHub REST endpoints the runner calls for one repository (repo, pulls, merge, issue comments, blocked-by, branch protection, check runs, Actions job logs) and a single-answer GraphQL stub. List endpoints honor `per_page` and `page`. Merge can be told to fail. |
| `cmd/fakeagent` | `cmd/fakeagent` | the `claude` and `grok` binaries |
| saved webhooks | `internal/control/testdata/*.json` | GitHub `issues` deliveries for intake tests |

Runner tests in `internal/runner` start a real embedded queue (`control.StartQueue` on a local port)
alongside these fakes.

## End-to-end journeys

`internal/e2e/e2e_test.go` carries the `e2e` build tag, so `make test` skips it. `TestMain` builds `schmeide` and
`fakeagent` into a temp directory and writes `claude` and `grok` wrapper scripts there: the login check
(`claude auth status`, `grok models`) answers logged in, and anything else runs `fakeagent` with the script from
`CLAUDE_E2E_SCRIPT` or `XAI_E2E_SCRIPT`. Those prefixes reach the session because the runner passes `CLAUDE_*`
and `XAI_*`.

Each journey (`newWorld`):

1. Seeds a local bare origin with a `.schmeide/config.toml` holding the gate under test.
2. Starts `fakegh` and a token minter `httptest` server.
3. Writes a fresh RSA App key (mode 0600) and starts `schmeide control` as a subprocess on free ports.
4. Starts `schmeide runner` subprocesses with `SCHMEIDE_TRUST=1`, `--github-url file://…/origin`,
   `--github-api-url` pointing at `fakegh`, and `--ack-wait 3s`. Claude Code is the writer and Grok the reviewer.
5. Posts a signed `labeled` webhook and waits on comments and pull requests in `fakegh`.

| Journey | Checks |
| --- | --- |
| `TestJourneyLabeledIssueOpensOnePullRequest` | one PR on `1_add-a-greeting` against `main` with the session's title and `Closes #1`, the branch pushed, review passed |
| `TestJourneyGateFailureCommentsAndOpensNoPullRequest` | gate `false` comments and opens no PR |
| `TestJourneyKilledRunnerJobIsRedeliveredWithoutASecondPullRequest` | runner killed during review, the job redelivered to a second runner after the 3s lease, still one PR |

## Renovate

`renovate.json` extends `github>autobutler-org/renovate-config`. The update policy lives in that shared repository.

## Keeping these pages true

A structural change updates the page that owns it in the same pull request:

| Change | Update |
| --- | --- |
| New package, or a package taking on a new responsibility | the file table on [Runner](runner.md) or [Control node](control-node.md) |
| New stream, bucket, subject, consumer, or permission | [Control node](control-node.md) and [Data](data.md) |
| New job state or stage | [Control node](control-node.md#job-states), [Runner](runner.md), [Data](data.md#job-stage-record) |
| New flag or config key that changes a flow | the flow in [Job flows](job-flows.md), and the [config reference](../config.md) for a repo key |
| New credential, or a change to what a process can see | [Credentials](credentials.md) |
| New command, Makefile target, workflow, or fake | this page |

When a page is added, add it to the index table in [README.md](README.md) and to the "Read next" table in
[`ARCHITECTURE.md`](../../ARCHITECTURE.md).
