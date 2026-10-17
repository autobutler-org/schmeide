# Repo config reference

Each repo that schmeide works on tracks its config in `.schmeide/config.toml`
at the repo root. The file is TOML. Every value is a quoted string. A key the
file does not recognize is an error, so a typo fails loudly instead of being
ignored.

A minimal file:

```toml
gate = "make check test"
```

A file that sets every key:

```toml
gate = "make check test"
merge = "factory"
check_wait = "10m"
gate_timeout = "30m"
session_timeout = "2h"
```

## Keys

| Key | Required | Default when absent or empty | Example |
| --- | --- | --- | --- |
| `gate` | yes | none; the file is rejected | `gate = "make check test"` |
| `merge` | no | a person merges (same as `"human"`) | `merge = "factory"` |
| `check_wait` | no | `2m` | `check_wait = "10m"` |
| `gate_timeout` | no | the runner's `--gate-timeout`, which defaults to `30m` | `gate_timeout = "30m"` |
| `session_timeout` | no | the runner's `--session-timeout`, which defaults to `2h` | `session_timeout = "2h"` |

### gate

The shell command a job must pass before schmeide opens a pull request. A
failing gate fails the job and is not retried. The key is required. An absent
or blank value is an error.

```toml
gate = "make check test"
```

### merge

Who merges a pull request after review has passed and CI is green.

- `"human"` leaves the pull request for a person.
- `"factory"` has the runner merge it once every check run on the head commit
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

Absent, empty, and blank values mean `"human"`. Any other value is an error,
and values are case-sensitive: `"Factory"` is rejected.

```toml
merge = "factory"
```

### check_wait

How long a factory merge waits for the head commit's check runs to succeed,
including the fix round's new head.
A Go duration such as `90s`, `10m`, or `1h30m`; it must be positive. Absent
or empty means `2m`. The key only matters when `merge = "factory"`. If the wait
ends without success, the pull request stays open.

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

## Runner flags

Two runner flags set the same deadlines for every repo that runner serves:

| Flag | Default | Overridden by |
| --- | --- | --- |
| `--gate-timeout` | `30m` | `gate_timeout` |
| `--session-timeout` | `2h` | `session_timeout` |

When the repo sets the key, the repo value wins over the flag. When the key is
absent or empty, the flag applies. For example, a runner started with
`--gate-timeout 1h` uses 1 hour for a repo with no `gate_timeout`, and 30
minutes for a repo with `gate_timeout = "30m"`.

`gate`, `merge`, and `check_wait` have no runner or control flag. They come
from the repo config alone.

## Errors

Every rejected value produces an error that names the file and the key and
shows a valid example. For example:

```text
.schmeide/config.toml: check_wait = "soon" is not a positive duration; set a duration such as check_wait = "10m"
.schmeide/config.toml: gate_timeout = "0s" is not a positive duration; set a duration such as gate_timeout = "30m"
.schmeide/config.toml: merge = "Factory" is not a legal value; set merge = "factory" or merge = "human"
.schmeide/config.toml: check_wait must be a quoted string; set it like check_wait = "10m"
.schmeide/config.toml: unknown key(s) gaet; remove them or fix the spelling
```
