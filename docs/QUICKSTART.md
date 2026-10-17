# Quickstart

This takes you from nothing to one pull request opened by schmeide, using one control node and one runner on the same machine. For the full design, see [design.md](design.md). For every flag, run `schmeide --help`.

The schmeide GitHub App is already installed on autobutler-org. This guide is for setting schmeide up in another org, with an App of your own. Creating the App is manual: schmeide does not automate it.

## What you need

- A GitHub org you can add an App to, and a repository in it.
- A public HTTPS URL that reaches this machine. The webhook listener is plain HTTP, so put a TLS-terminating proxy in front of it.
- `git` on `PATH`, and one harness (Claude Code or the Grok CLI) installed and logged in. schmeide does not handle agent API keys.
- A built `schmeide` binary (`make build` gives `./build/schmeide`). To install the binary instead, see the Install section of the [README](../README.md) (`make install`, or `go install` of a tag).

## 1. Create the GitHub App

In the org, go to Settings, Developer settings, GitHub Apps, New GitHub App.

- **Webhook URL:** `https://<your-host>/webhook`.
- **Webhook secret:** a long random string. Keep it; the control node needs the same value.
- **Repository permissions:** Contents: read and write. Pull requests: read and write. Issues: read and write. Checks: read-only.
- **Subscribe to events:** Issues.
- Create the App, then note its **App ID**.
- Generate a **private key** and save the `.pem` file.
- Install the App on your org, on the repository you will test with.

The private key must not be readable by group or other. Control refuses to start otherwise:

```sh
chmod 0600 /path/to/app.pem
```

## 2. Add the repo config

`schmeide init` writes `.schmeide/config.toml` and `.schmeide/control.conf` in the current directory, and it refuses to overwrite either file. `.schmeide/control.conf` is an operator config with placeholders, mode 0600, and must not be committed with real secrets. Pass it with `schmeide control --config .schmeide/control.conf` after you replace every placeholder.

The target repository needs `.schmeide/config.toml` on its default branch:

```toml
# Required. The command a job must pass before a pull request is opened.
gate = "make check test"

# Optional. Absent, empty, and "human" leave the pull request for a person.
# "factory" lets the factory merge after review and green checks.
merge = "human"
```

A missing `merge` setting leaves the pull request for a person to merge. `gate` is required, and unknown keys are an error. Every key is listed in [config.md](config.md).

## 3. Start the control node

The webhook secret, App ID, key file, runner tokens, and an allowlist are all required. The allowlist must not be empty: set `--allowed-orgs`, `--allowed-installations`, or both, or control refuses to start. Put secrets in the environment so they stay out of the process list.

```sh
export SCHMEIDE_WEBHOOK_SECRET=...
export SCHMEIDE_GITHUB_APP_ID=...
export SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE=/path/to/app.pem
export SCHMEIDE_RUNNER_TOKENS='octocat:a-token-for-octocat'
export SCHMEIDE_ALLOWED_ORGS=your-org
schmeide control
```

`--runner-tokens` is `<github login>:<token>`, one entry per steward. The listener serves `POST /webhook` on `--http-addr` (default `:8080`). Point your TLS proxy at it. The runner connects to NATS on `--nats-addr` (default `127.0.0.1:4222`).

## 4. Check, then start the runner

On the runner machine, with the login and token from the previous step:

```sh
schmeide doctor --steward octocat --nats-token a-token-for-octocat
```

Doctor checks that git is on `PATH`, that at least one harness is installed and logged in, and that the control node answers for that steward. Each failed check says how to fix it. When it passes:

```sh
schmeide runner --steward octocat --nats-token a-token-for-octocat
```

An unattended runner needs `SCHMEIDE_TRUST=1` the first time it uses a work directory, and on any later run of a directory and origin that have no recorded yes. The variable lets that run proceed and records nothing. On a terminal the runner asks once for a new pair and stores a yes under the user config directory (`schmeide/trust`). A non-interactive run with no recorded decision and without `SCHMEIDE_TRUST=1` refuses the job.

## 5. Get one pull request

On an open issue in the repository, assign it to the steward's login (`octocat` above) and add the label `schmeide:ready`. Both the assignment and the label are needed, in either order.

The webhook creates one job. The runner takes it, works on a branch named `<issue number>_<short-description>`, runs the gate from `.schmeide/config.toml`, then pushes and opens a pull request as the App. A review stage then runs. With the default `merge`, a person presses merge.

## Shell completion

`schmeide completion <shell>` prints a completion script to stdout.

```sh
# bash: add to ~/.bashrc
source <(schmeide completion bash)

# zsh: add to ~/.zshrc, after compinit
source <(schmeide completion zsh)

# fish
schmeide completion fish > ~/.config/fish/completions/schmeide.fish
```

## Troubleshooting

- **Control will not start:** check the allowlist is set and the key file is mode 0600.
- **No job after labeling:** check the delivery in the App's Advanced tab, and that the issue is assigned, labeled, and in an allowed org. A repeat delivery within `--delivery-ttl` is not published again.
- **A failure before the pull request opens** is retried up to `--max-attempts` (default 3). The count is in memory, so a restart starts it over. A gate failure is commented on the issue and is not retried.
- Container isolation is not built. The harness and gate run as the user that started the runner, so use a user that holds nothing else worth taking.
