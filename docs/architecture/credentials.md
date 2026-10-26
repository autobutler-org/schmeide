# Credentials

This page lists every secret schmeide handles, who holds it, how far it reaches, and which process can see it.
The design rule is that a runner holds no long-lived GitHub credential and schmeide never handles agent keys.

## Trust boundaries

```mermaid
flowchart TB
    subgraph ctl["Control node process"]
        key["App private key"]
        wsec["webhook secret"]
        rtok["all runner tokens"]
        cpw["control NATS password (random per start)"]
    end
    subgraph runner["Runner process (steward's OS user)"]
        ntok["this steward's NATS token"]
        jtok["current job's installation token<br/>(jobToken: asks again 5 min before expiry)"]
        subgraph git["git commands"]
            gtok["token via GIT_CONFIG_VALUE_0"]
        end
        subgraph sess["harness session (own process session)"]
            senv["base env + ANTHROPIC_, CLAUDE_, XAI_ + --pass-env<br/>no SCHMEIDE_*, no job token"]
        end
        subgraph gate["gate command (own process session)"]
            genv["base env + --gate-pass-env<br/>no SCHMEIDE_*, no job token<br/>HOME and TMPDIR at a per-job scratch dir"]
        end
    end
    gh["GitHub"]
    ctl -- "JWT signed with App key" --> gh
    ctl -- "TokenReply over NATS (TLS)<br/>at claim, then whenever the held one is near expiry" --> runner
    git -- "Authorization: Basic x-access-token" --> gh
    runner -- "Bearer job token (REST, GraphQL)" --> gh
```

| Boundary | What crosses it | What is kept out |
| --- | --- | --- |
| GitHub to control node | signed webhook payloads | anything without a valid `X-Hub-Signature-256` |
| Control node to runner | job messages, a token for each of the steward's own jobs (and pool jobs for a `--pool` runner) | the App key, the webhook secret, other stewards' jobs, tokens, stage records, registrations, and job statuses |
| Runner to harness session | the brief, the worktree, the reduced environment | the job's installation token, the NATS token, `SCHMEIDE_*` settings |
| Runner to gate | the worktree, the base environment, and `--gate-pass-env` | the job's installation token (it is only in git's environment), the NATS token, `SCHMEIDE_*` settings, the agent logins, the runner's `HOME` |
| Worktree to remote | the branch's commits, whose added lines pass the secret scan | credential-shaped literals, the job's installation token, the runner's NATS token |

The session and the gate run as the runner's OS user, with that user's files. There is no container or OS
sandbox. [FAILURE-MODES.md](../FAILURE-MODES.md#not-built) and [design.md](../design.md#open-decisions) say so.

## GitHub App installation tokens

| Aspect | Value |
| --- | --- |
| Minted by | `control.TokenMinter.Mint` (`internal/control/token.go`) |
| Authenticated as | the App, with an RS256 JWT: `iss` App ID, `iat` now minus 1 minute, `exp` now plus 9 minutes |
| Endpoint | `POST <github-api-url>/app/installations/<installation_id>/access_tokens` |
| Repository scope | `repository_ids: [job.repo_id]`, the one repository |
| Permissions requested | `contents: write`, `pull_requests: write`, `issues: write`, `checks: read`, `administration: read`, `actions: read` |
| Lifetime | one hour, set by GitHub. The first token of a delivery is requested at claim time; the runner asks for another whenever the one it holds is within five minutes of its `expires_at`. The one held when the delivery ends is revoked. |
| Who may ask | a runner on `schmeide.token.<steward>`, for a subject in `SCHMEIDE_JOBS` that is that steward's job, or a pool job when the steward registered with `--pool` (`Queue.tokenFor`) |
| Where the runner keeps it | in memory only, in the job's `jobToken` (`internal/runner/token.go`); `forge.GitHub.Tokens` and every git command read it from there |

How the runner uses it:

- Git: `worktree.git` (`internal/runner/workspace.go`) asks the job's token source for the current token and sets
  `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=http.extraHeader`, and
  `GIT_CONFIG_VALUE_0=Authorization: Basic base64(x-access-token:<token>)` in the environment of each git command
  it runs. The token is never written to git config or put on a command line. `GIT_TERMINAL_PROMPT=0` stops a
  credential prompt.
- REST and GraphQL: `Authorization: Bearer <token>`, built by `forge.GitHub.Bearer` before each call from
  `forge.GitHub.Tokens` (`internal/forge/github.go`, `internal/forge/queue.go`, and `RetargetPull` in
  `internal/runner/github.go`).

### Revoking a delivery's token

When the delivery ends, the runner revokes the token it holds with `DELETE <github-api-url>/installation/token`,
authenticated with that token (`github.RevokeToken` in `internal/runner/github.go`, called from `Runner.revoke`
with `jobToken.current`, which does not refresh). The call is deferred as soon as the first token arrives, so it
runs after the outcome comment and the `Ack` of a final state, and also after a `retrying` hand-back, a stuck-job
drop, and a shutdown hand-back: the next delivery mints a fresh token. A token that leaked during the job
(through the gate, a log, or a commit) is then useful for the job's own length, not the full hour. Only the
current token is revoked: a token a refresh replaced was within five minutes of its expiry and expires on its own.
A failed revoke is logged as "could not revoke the job's token; it expires within the hour" and does not change
the job's outcome; the token then lives until GitHub expires it.

### Refreshing a job's token

A job can run longer than a token lives: the default `session_timeout` alone is two hours, and review rounds and
`check_wait` come after it. So the runner never holds one token for a whole job.

```mermaid
sequenceDiagram
    participant J as runner: jobToken.get
    participant Q as control: Queue.tokenFor
    participant GH as GitHub
    Note over J: claim: no token held
    J->>Q: request schmeide.token.{steward} with the job subject
    Q->>GH: POST /app/installations/{id}/access_tokens
    GH-->>Q: token, expires_at (1 hour)
    Q-->>J: TokenReply
    Note over J: each GitHub call and git command calls get
    alt now is before expires_at minus 5 minutes
        J-->>J: return the held token
    else within 5 minutes of expires_at
        J->>Q: request again with the same job subject
        Q->>GH: mint a new token
        Q-->>J: TokenReply
        J-->>J: hold and return the new token
    end
```

- `Runner.handle` builds one `jobToken` per delivery (`internal/runner/token.go`). Its `fetch` is the same
  `schmeide.token.<steward>` request the claim makes. `get` holds a mutex, so concurrent calls refresh once.
- `stale` decides: no token yet, or `now` at or after `expires_at` minus `tokenRefreshMargin` (five minutes). A
  token without an `expires_at` is never refreshed.
- `Queue.tokenFor` (`internal/control/queue.go`) mints a new token on every request whose subject is still in
  `SCHMEIDE_JOBS` and belongs to the asking steward (or is a pool job it may take). The job stays in the stream
  while it is worked and after it is acknowledged (the stream has limits retention), so a refresh at any point of
  the job, including the outcome comment, is answered. Each refresh is one more token minted by the App.
- A refresh that fails returns `refresh the job's GitHub token: <cause>` and keeps the old token, so the next call
  tries again. The GitHub call or git command that needed it fails with that error and the job fails the way any
  GitHub error fails it. The outcome comment is still attempted after that, with its own refresh. When that one
  fails too, the runner logs `level=ERROR msg="could not comment on the issue"` with the refresh error and
  acknowledges the job; the job's state is still reported on `SCHMEIDE_STATUS`, so `schmeide status` shows it.

### What each permission is for

| Permission | Used for |
| --- | --- |
| `contents: write` | fetch and push the job's branch, merge the pull request |
| `pull_requests: write` | open, find, retarget, and merge pull requests |
| `issues: write` | read the issue's comments and blockers, post the outcome comment |
| `checks: read` | list check runs on the pull request's head |
| `administration: read` | `GET /repos/{owner}/{repo}/branches/{branch}/protection` in `forge.GitHub.Protection`, before a factory merge |
| `actions: read` | `GET /repos/{owner}/{repo}/actions/jobs/{job_id}/logs` in `forge.GitHub.JobLog`, for a CI fix round |

GitHub's "Permissions required for GitHub Apps" REST page lists the protection read under Administration (read)
and the job log download under Actions (read). The request is pinned by `TestTokenPerJob` (`internal/control`).

GitHub cannot grant an installation token a permission the App installation was not granted. An App created
before `administration: read` and `actions: read` were requested, or an installation whose owner has not accepted
the App's new permissions, gets no token at all: every job is handed back as in JN-FAIL-015 until the App has
both permissions. `forge.GitHub.Protection` still treats a 403 on the protection read as no protection, and when
the job log read fails, `logTail` puts "(schmeide could not read the log ...)" in the fix brief in place of the
log.

## Per-steward NATS tokens

`--runner-tokens` on the control node is `<login>:<token>,...`. `ParseRunnerTokens` refuses an empty list, a
duplicate steward, a steward with subject syntax or `/` or `\`, and `_pool`. Each entry becomes a NATS user named
after the steward with the token as its password and `runnerPermissions(steward)` as its permissions (see
[Control node](control-node.md#runner-permissions)). A runner, `schmeide status`, and `schmeide doctor` pass the
same pair as `--steward` and `--nats-token`.

What a stolen steward token gives: that steward's jobs (including issue text), its token subject (and so
installation tokens for its jobs' repositories), its stage records, pool jobs and their stage records and
statuses, its own job statuses, and its own registrations. It does not give other stewards' jobs, tokens, replies,
registrations (host names, harnesses, versions), or job statuses (which can carry gate output): `schmeide status`
asks the control node on `schmeide.status-query.<steward>`, and the reply holds only that steward's runners and
jobs, plus pool jobs when the steward has a live pool runner. Stream info on `SCHMEIDE_STATUS` and
`KV_SCHMEIDE_STAGES` stays allowed, because a runner opens both by name; stream info can list subject names, so a
steward can still learn which stewards have registered and which repository IDs and issue numbers have statuses,
but not what those messages say. Token rotation is a control node restart with a new `--runner-tokens`.

The control node's own connection is in-process, as `_schmeide_control` with a password from `rand.Text()` that
is never written anywhere.

Prefer `SCHMEIDE_RUNNER_TOKENS` and `SCHMEIDE_NATS_TOKEN` (or the `--config` file) over flags, so the tokens stay
out of the process list. The flag help says the same.

## Webhook secret

`--webhook-secret` (`SCHMEIDE_WEBHOOK_SECRET`) is the App's webhook secret. `VerifySignature` computes
HMAC-SHA256 of the raw body and compares it with `hmac.Equal` against the `sha256=` header. A bad or missing
signature is a 401 before anything in the body is trusted. The allowlist (`--allowed-orgs`,
`--allowed-installations`) is a second check after the signature, so a valid delivery from an installation
outside the list is a 403 when it would create a job.

## App private key file

`--github-app-private-key-file` names a PEM file holding a PKCS#1 or PKCS#8 RSA key. `CheckPrivateKeyFileMode`
refuses to start when the mode has any group or other read bit (`mode & 0o044`). The key is read once in
`control.Run` and kept in memory for JWT signing. It never leaves the control node.

## Harness session environment

`harnessEnv` in `internal/runner/env.go` filters the runner's `os.Environ()`:

| Kept | Names |
| --- | --- |
| base names (`baseEnv`) | `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `LANG`, `TZ`, `TMPDIR`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, `http_proxy`, `https_proxy`, `no_proxy`, `SSL_CERT_FILE`, `SSL_CERT_DIR` |
| base prefixes (`basePrefixes`) | `LC_*`, `XDG_*` |
| harness prefixes (`harnessPrefixes`) | `ANTHROPIC_*`, `CLAUDE_*`, `XAI_*`: the harness's own login and settings |
| `--pass-env` | each listed name, exactly. The help text says never to list a secret. |
| added by the runner | `GOCACHE`, `GOMODCACHE`, `GOFLAGS` (existing value plus `-modcacherw`) |

Everything else is dropped, including every `SCHMEIDE_*` variable, so a session cannot read the steward's NATS
token from its environment. The job's installation token is never in the session's environment either: it lives
only in the git commands' environment. A session can still read any file the runner's OS user can read, including
the harness's own login under `HOME`.

### Gate environment

`gateEnv` in `internal/runner/env.go` filters the runner's `os.Environ()` for the gate, and `runner.work` passes
the result to `newWorktree` as `worktreeSpec.gateEnv`:

| Kept | Names |
| --- | --- |
| base names (`baseEnv`) | the same list as the harness session |
| base prefixes (`basePrefixes`) | `LC_*`, `XDG_*` |
| `--gate-pass-env` | each listed name, exactly. The help text says never to list a secret. |
| added by `worktree.Run` | `HOME` and `TMPDIR` set to the job's scratch directory, then `GOCACHE`, `GOMODCACHE`, `GOFLAGS` |

The harness prefixes (`ANTHROPIC_*`, `CLAUDE_*`, `XAI_*`) are not kept, so the gate does not see the agent
logins. A `SCHMEIDE_*` name is never kept, even when listed: `gatePassEnv` in `internal/runner/cli.go` refuses one
in `--gate-pass-env` at startup with `--gate-pass-env lists <NAME>: the gate never sees a SCHMEIDE_ name` (exit
code 2), and `gateEnv` drops any that slip through. `scratchHome` drops `HOME`, `TMPDIR`, and the
`XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`, and `XDG_STATE_HOME` variables, then points `HOME` and
`TMPDIR` at `<work-dir>/scratch/<repo_id>-<issue>`, so the gate reads no dotfiles from the runner's home and the
XDG directories default to under the scratch `HOME`. `newWorktree` clears and recreates that directory (mode
`0700`) for each job, and `Close` removes it with the worktree. The gate still runs as the runner's OS user, so it
can read any file that user can read by absolute path. Run the runner as a user that holds nothing else worth
taking.

Git commands get only the token and identity variables (`GIT_TERMINAL_PROMPT`, `GIT_CONFIG_*`, `GIT_AUTHOR_*`,
`GIT_COMMITTER_*`), with no `PATH` or `HOME`.

## Harness permission flags

`--claude-args` and `--grok-args` default to `--permission-mode acceptEdits`, which lets a session edit files in
its worktree. `harnessFlags.check` in `internal/runner/cli.go` refuses to start when either contains
`--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, or a `--permission-mode` other than
`acceptEdits` (`harness.WideningArg`), unless `--allow-dangerous-harness-args` is set. `--claude-args` reaches only
the Claude Code harness and `--grok-args` only Grok. Details and the opt-in are in
[permissions.md](../permissions.md).

## TLS material

`--nats-tls-cert-file` and `--nats-tls-key-file` are read by `natsTLSConfig` at startup (a bad pair fails startup)
and again on every TLS handshake, so a renewed certificate is served without a restart. A failed reload keeps the
last good pair. Runners verify the server certificate against the system roots (`SSL_CERT_FILE` for a private CA
on Linux). There are no client certificates; the steward token is the client credential, so `control.Run` refuses
to start when NATS listens beyond loopback without TLS (`checkNATSTransport` in `internal/control/tls.go`, which
returns `ErrPlaintextNATS`). `--allow-plaintext-nats true` is the operator's explicit opt-in to plain text; with it
the control node starts and logs a warning. The webhook listener has no TLS of its own and relies on the proxy in
front of it.

## Secret scan before push

`secretscan.ScanDiff` runs before every `git push` from `Workspace.Push` and `Workspace.Restack`. It reads only
the lines the branch adds relative to its base: `git diff -U0 -M <base>...HEAD`, run with the real git binary and
a minimal environment (`PATH` and `GIT_CONFIG_GLOBAL=/dev/null`), so the scan neither hands the runner's secrets
to git nor reads the process environment. The three-dot form diffs from the merge base, so commits the base
gained later are not read, and `-M` keeps a renamed file's unchanged lines out. It matches:

| Kind | Pattern |
| --- | --- |
| PEM private key | `-----BEGIN ... PRIVATE KEY-----` |
| GitHub token | `ghp_`, `gho_`, `ghu_`, `ghs_` (installation), or `ghr_` plus 36 or more, or `github_pat_` plus 20 or more |
| GitLab token | `glpat-` plus 20 or more |
| AWS access key | `AKIA` plus 16 |
| JWT | `eyJ` and three base64url segments of 8 or more |
| Anthropic API key | `sk-ant-` plus 20 or more |
| xAI API key | `xai-` plus 20 or more |
| runner credential | the job's installation token or the runner's NATS token, verbatim (8 characters or more) |

The NATS token has no fixed shape, so the runner passes it and the job's installation token to the scan, which
refuses an exact match of either. The Anthropic and xAI keys are the ones `harnessEnv` hands the session through
`ANTHROPIC_*` and `XAI_*`; the runner does not read their values, so only their shapes are matched.

A line containing `schmeide:allow-secret` is skipped by the patterns. The marker does not excuse an exact match
of a runner credential. A finding refuses the push and names file, line, and kind, never the text. The refusal
is a failed work outcome. A literal that is already on the base does not block the job, so a repository that
contains one can still be pushed to by the factory. `secretscan.Scan`, which reads every tracked file, is kept
for the schmeide repository's own hygiene test (`TestTrackedFilesHaveNoCredentialLiterals`). `.gitguardian.yaml`
lists the same shapes in a comment for the GitGuardian scan on the schmeide repository itself.

## Trust prompt

The trust prompt is a guard against running sessions and gates for an origin nobody agreed to, not a credential.
It is described in [Runner](runner.md#trust-prompt). `SCHMEIDE_TRUST=1` disables it for one process.
