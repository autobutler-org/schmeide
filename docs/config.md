# Repo config reference

Each repo that schmeide works on tracks its config in `.schmeide/config.toml`
at the repo root. The file is TOML. Every value is a quoted string. A key the
file does not recognize is an error, so a typo fails loudly instead of being
ignored.

The runner reads the file from the base branch before any harness session. A
missing file, or any error below, fails the job finally: one comment on the
issue names the file and the error, no pull request opens, and the job is not
retried. Push a fixed file and add the ready label again to make a new job.

A minimal file:

```toml
gate = "make check test"
```

A file that sets every key:

```toml
gate = "make check test"
check_wait = "10m"
gate_timeout = "30m"
session_timeout = "2h"

[automate]
merge = "on"
review_fix = "on"
create_issues = "off"
```

## Keys

| Key | Required | Default when absent or empty | Example |
| --- | --- | --- | --- |
| `gate` | yes | none; the file is rejected | `gate = "make check test"` |
| `check_wait` | no | `2m` | `check_wait = "10m"` |
| `gate_timeout` | no | the runner's `--gate-timeout`, which defaults to `30m` | `gate_timeout = "30m"` |
| `session_timeout` | no | the runner's `--session-timeout`, which defaults to `2h` | `session_timeout = "2h"` |
| `automate.merge` | no | `"off"`: a person merges | `[automate] merge = "on"` |
| `automate.review_fix` | no | `"off"`: no fix rounds for review findings | `[automate] review_fix = "on"` |
| `automate.create_issues` | no | `"off"`; nothing reads it yet | `[automate] create_issues = "off"` |

### gate

The shell command a job must pass before schmeide opens a pull request. A
failing gate fails the job and is not retried. The key is required. An absent
or blank value is an error.

```toml
gate = "make check test"
```

### check_wait

How long a factory merge waits for the head commit's check runs to succeed,
including the fix round's new head.
A Go duration such as `90s`, `10m`, or `1h30m`; it must be positive. Absent
or empty means `2m`. The key only matters when `automate.merge = "on"`. The runner
reads the check runs after one second, then backs off, doubling the pause up to
30 seconds, and reads them once more at the end of the wait. If the wait ends
without success, the pull request stays open, and the comment names the check
that was not green and the wait. A stacked pull request gets a wait of its own
for the pull request below it to merge, before this one.

```toml
check_wait = "10m"
```

### gate_timeout

How long the gate command may run before its process group is canceled and
the job fails. A positive Go duration. Absent or empty keeps the runner's
`--gate-timeout`. A gate timeout is not retried.

```toml
gate_timeout = "30m"
```

### session_timeout

How long one harness session may run before its process group is canceled
and the job fails. A positive Go duration. Absent or empty keeps the runner's
`--session-timeout`. A session timeout before the pull request opens counts
toward the runner's `--max-attempts`.

```toml
session_timeout = "2h"
```

## [automate]

The `[automate]` table holds switches for what the factory does without a
person. Every switch is `"on"` or `"off"`. Absent, empty, and blank values mean
`"off"`, so a repo with no `[automate]` table leaves every step after review to
a person. Any other value is an error, and values are case-sensitive: `"On"`
and `"yes"` are rejected. An unknown key inside the table is an error too.

```toml
[automate]
merge = "on"
review_fix = "on"
create_issues = "off"
```

The runner reads the table with the gate, before the harness session, so the
session cannot turn a switch on for itself. The top-level `merge` key is gone:
a file that still sets it is rejected and told to move it into `[automate]`.

### automate.merge

Whether the runner merges a pull request after review has passed and CI is
green.

- `"off"` leaves the pull request for a person.
- `"on"` has the runner merge it once every check run on the head commit
  has succeeded (see `check_wait`). Before the wait, the job fails if the
  base branch's protection, when the runner can read it, requires one or more
  approving reviews: the runner cannot supply that review, the pull request
  stays open, and nothing is merged or enqueued. A 404 or a 403 on the read
  does not fail the job. Any other read failure fails the job, and the pull
  request stays open. Required status checks are not this failure.
  - The first red check runs one writer fix round when a review round
    remains. It counts against `--max-review-rounds`. There is no second fix
    round. A check that is still red, or a wait that ends without success,
    leaves the pull request open.
  - When the base branch has a merge queue, green checks add the pull request
    to the queue instead of merging it. The job is reported queued.
  - A pull request stacked on another schmeide pull request merges bottom
    first. Before the check wait it waits, for up to `check_wait`, for the pull
    request below to merge, rebasing onto that branch as it moves. Once it
    merged, the runner retargets this pull request to the default branch, if
    GitHub has not, and rebases it onto the default branch. A bottom that has
    not merged by then, one that closed without merging, or a rebase that
    conflicts leaves the pull request open, and the comment says which.
  - A failure after the checks are green starts with "CI is green, but", so a
    pull request that looks ready says why it was not merged.

```toml
[automate]
merge = "on"
```

### automate.review_fix

Whether the writer gets fix rounds for review findings.

- `"on"`: when the reviewer requests changes and a round of
  `--max-review-rounds` remains, a fresh worker session applies the findings,
  the gate runs again, and the same branch is pushed so the open pull request
  updates. Exhausting the rounds leaves the pull request open.
- `"off"`: the reviewer still runs once and its result is reported. A pass
  finishes the review as usual. A request for changes gets no fix round: the
  job fails, the comment says the review requested changes and
  `automate.review_fix` is off, and the pull request stays open for a person.

Factory merge still happens only after review passes, so with `merge = "on"`
and `review_fix = "off"` a rejected pull request is not merged. The CI fix
round under `automate.merge` is not a review fix and still runs.

```toml
[automate]
review_fix = "on"
```

### automate.create_issues

Accepted ahead of the feature. schmeide does not create issues yet, and
nothing reads this switch. It is validated like the other switches.

```toml
[automate]
create_issues = "off"
```

## Runner flags

Runner flags set delivery and execution deadlines for every repo that runner
serves:

| Flag | Default | Overridden by |
| --- | --- | --- |
| `--ack-wait` | `5m` | the shared consumer's `AckWait`, when another runner created it first; the runner heartbeats every third of the lease in force |
| `--stuck-after` | `3h` | never; a state change resets it |
| `--gate-timeout` | `30m` | `gate_timeout` |
| `--session-timeout` | `2h` | `session_timeout` |

When the repo sets the key, the repo value wins over the flag. When the key is
absent or empty, the flag applies. For example, a runner started with
`--gate-timeout 1h` uses 1 hour for a repo with no `gate_timeout`, and 30
minutes for a repo with `gate_timeout = "30m"`.

`gate`, `check_wait`, and the `[automate]` switches have no runner or control
flag. They come from the repo config alone.

`--gate-pass-env` (`SCHMEIDE_GATE_PASS_ENV`) is a comma-separated list of
variable names the gate sees on top of the base list. It defaults to empty. A
`SCHMEIDE_` name is refused at startup with `--gate-pass-env lists <NAME>: the
gate never sees a SCHMEIDE_ name`. The gate's `HOME` and `TMPDIR` are always a
per-job scratch directory under `--work-dir`. Never list a secret. See
[architecture/credentials.md](architecture/credentials.md#gate-environment) and
JN-PR-021.

`ack-wait` is the JetStream lease for a claimed job. A runner must stay
connected and send `InProgress` heartbeats during long work. `stuck-after` is
the longer no-progress budget: when it expires the runner cancels the work and
Naks the job for redelivery. They are operator settings, not repository keys.

`ack-wait` is applied only by the runner that creates a consumer. All of a
steward's runners share one consumer, and all `--pool` runners share another,
so give every runner that shares a consumer the same `--ack-wait`. A runner
whose value differs logs a warning and heartbeats at the consumer's lease. See
[FAILURE-MODES.md](FAILURE-MODES.md#delivery-heartbeats-and-stuck-jobs) for how
to change the lease.

## Errors

Every rejected value produces an error that names the file and the key and
shows a valid example. For example:

```text
.schmeide/config.toml: check_wait = "soon" is not a positive duration; set a duration such as check_wait = "10m"
.schmeide/config.toml: gate_timeout = "0s" is not a positive duration; set a duration such as gate_timeout = "30m"
.schmeide/config.toml: automate.merge = "yes" is not a legal value; set automate.merge = "on" or automate.merge = "off"
.schmeide/config.toml: check_wait must be a quoted string; set it like check_wait = "10m"
.schmeide/config.toml: automate.review_fix must be a quoted string; set it like automate.review_fix = "on"
.schmeide/config.toml: unknown key(s) gaet; remove them or fix the spelling
.schmeide/config.toml: merge moved into [automate]; set it as [automate] merge = "on"
```
