# Harness permissions

Every harness session starts from the same floor, for Claude Code and Grok
alike:

- A session may edit files in its own worktree.
- A session is not handed flags that skip permission checks.

The default session flags hold that floor. Both `--claude-args` and
`--grok-args` default to `--permission-mode acceptEdits`.

Sessions run in `--print` mode, where no one can answer a permission prompt.
Under `acceptEdits`, Claude Code refuses every Bash tool call ("This command
requires approval"), so a session edits files but cannot run the repo's
formatter, linter, or tests, and its mistakes surface only at the gate. That
is deliberate. See
[FAILURE-MODES.md](FAILURE-MODES.md#sessions-cannot-run-commands-under-the-default-permission-floor)
and JN-FAIL-025.

## What the runner refuses

`schmeide runner` refuses to start when `--claude-args` or `--grok-args`
contains either of these:

- `--dangerously-skip-permissions` (or `--allow-dangerously-skip-permissions`)
- a `--permission-mode` other than `acceptEdits`, such as `bypassPermissions`

The error names the flag it found. Other flags, such as `--verbose`, are
not refused.

## Opting in

Pass `--allow-dangerous-harness-args` (or set
`SCHMEIDE_ALLOW_DANGEROUS_HARNESS_ARGS=true`) to accept those flags. For
example, `--claude-args "--permission-mode auto" --allow-dangerous-harness-args`
(or `SCHMEIDE_CLAUDE_ARGS="--permission-mode auto"`) lets Claude Code
sessions run commands. The
runner then starts as it would with any other session flags. A session with
skipped permission checks runs with every right of the runner's OS user, so
run the runner as a user that holds nothing else worth taking. Schmeide does
not add an OS sandbox.

`schmeide doctor` states this floor in its `permission floor` check. That
check does not fail.
