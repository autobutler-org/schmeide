# Harness permissions

Every harness session starts from the same floor, for Claude Code and Grok
alike:

- A session may edit files in its own worktree.
- A session is not handed flags that skip permission checks.

The default session flags hold that floor. Both `--claude-args` and
`--grok-args` default to `--permission-mode acceptEdits`.

## What the runner refuses

`schmeide runner` refuses to start when `--claude-args` or `--grok-args`
contains either of these:

- `--dangerously-skip-permissions` (or `--allow-dangerously-skip-permissions`)
- a `--permission-mode` other than `acceptEdits`, such as `bypassPermissions`

The error names the flag it found. Other flags, such as `--verbose`, are
not refused.

## Opting in

Pass `--allow-dangerous-harness-args` (or set
`SCHMEIDE_ALLOW_DANGEROUS_HARNESS_ARGS=true`) to accept those flags. The
runner then starts as it would with any other session flags. A session with
skipped permission checks runs with every right of the runner's OS user, so
run the runner as a user that holds nothing else worth taking. Schmeide does
not add an OS sandbox.

`schmeide doctor` states this floor in its `permission floor` check. That
check does not fail.
