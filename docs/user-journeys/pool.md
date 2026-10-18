# Pool journeys

Covers pool runners: runners started with `--pool` that also take jobs whose
steward has no registered runner. Implemented in `internal/control/queue.go`
(`Publish`, `tokenFor`), `internal/control/status.go` (registrations), and
`internal/runner/runner.go` (`next`). See
[../GLOSSARY.md](../GLOSSARY.md#pool).

Whether a job goes to the pool is decided once, when the control node
publishes it: a steward with no live registration gets their job on the pool.
A runner publishes its registration at startup and every minute
(`control.RegistrationInterval`), and a registration is live for three minutes
(`control.RegistrationTTL`) after it was stored. A runner that stops therefore
stops counting within three minutes.

---

### JN-POOL-001: Start a pool runner

**Preconditions:** JN-SETUP-010 passes. The control node lists this steward in
`--runner-tokens`.

**Steps:**

1. Run `schmeide runner --steward alice --nats-token <alice's token> --pool`
   (or set `SCHMEIDE_POOL=true`).

**Expected result:**

- The runner logs `msg="runner registered" steward=alice ... pool=true`.
- `schmeide status` shows the runner with `true` in the POOL column.

**Notes:**

- `--pool` is off by default.
- Covered by `TestPoolFlagDefaultsOff` and `TestTrackerRecordsRegisteredRunners`.

---

### JN-POOL-002: A steward with no runner gets a pool runner

**Preconditions:** JN-POOL-001 done (alice's pool runner). `bob` has never
started a runner against this control node.

**Steps:**

1. Assign an issue to `bob` and add `schmeide:ready`.

**Expected result:**

- alice's pool runner claims the job and opens a pull request as usual.
- The steward is still `bob`. The pull request is opened by the App, and the
  comments go on bob's issue.
- `schmeide status` lists the job under `schmeide.jobs._pool.<repo id>.<issue>`
  while it runs.

**Notes:**

- bob does not need an entry in `--runner-tokens` for this.
- The pool runner works the job on alice's machine, in alice's work directory,
  with alice's harness login and trust decisions (JN-SETUP-021).
- Covered by `TestUnregisteredStewardIsWorkedByAPoolRunner` and
  `TestPublishRoutesByRegistration`.

---

### JN-POOL-003: A steward with a running runner keeps the job

**Preconditions:** `bob` runs a runner without `--pool`. alice runs a pool
runner.

**Steps:**

1. Assign an issue to `bob` and add `schmeide:ready`.

**Expected result:**

- alice's pool runner does not take it. bob's runner takes it.

**Notes:**

- Covered by `internal/runner` `TestRegisteredStewardKeepsTheJobFromThePool`.

---

### JN-POOL-004: A pool job waits when no pool runner is running

**Preconditions:** No runner has `--pool`. `carol` has never registered.

**Steps:**

1. Assign an issue to `carol` and add `schmeide:ready`.
2. Start a runner for `carol` without `--pool`.
3. Stop it and start it again with `--pool`.

**Expected result:**

- Step 1: the delivery answers `created`, but no runner claims the job.
- Step 2: carol's runner does not take it either, because the job was put on
  the pool when it was published.
- Step 3: carol's pool runner takes it.

**Notes:**

- A runner without `--pool` never receives a pool job.
- Covered by `TestRunnerWithoutPoolLeavesThePoolJob`.

---

### JN-POOL-005: A pool runner takes its own steward's job first

**Preconditions:** alice's pool runner is idle. A pool job and one of alice's
own jobs are both waiting.

**Steps:**

1. Watch which job alice's runner claims first.

**Expected result:**

- It claims alice's own job first, and the pool job only when it has no own
  job waiting.

**Notes:**

- Covered by `TestPoolRunnerWorksItsOwnJobFirst`.

---

### JN-POOL-006: One job at a time, one runner per pool job

**Preconditions:** Two pool runners, alice's and dave's. One pool job.

**Steps:**

1. Assign and label an issue for a steward with no runner.

**Expected result:**

- Exactly one of the two runners claims it. Each runner works one job at a
  time, so a busy pool runner takes nothing more until it finishes.

**Notes:**

- All pool runners share one queue position, so each pool job goes to one of
  them.
- Covered by `TestPoolRunnersShareOneJob`.

---

### JN-POOL-007: A pool job takes the same path as any job

**Preconditions:** JN-POOL-002.

**Steps:**

1. Follow the job to its outcome.

**Expected result:**

- It gets a token limited to the job's repository, runs the repo's gate, and
  goes through review and, with `merge = "on"`, factory merge, exactly as in
  [issue-to-pr.md](issue-to-pr.md), [review.md](review.md), and
  [merge.md](merge.md). Retries, timeouts, and redelivery work as in
  [failures.md](failures.md).

**Notes:**

- The control node gives a pool job's token only to a steward whose runner
  registered with `--pool`. A runner that pulls a pool job without it gets no
  token and hands the job back.
- Covered by `TestPoolJobTokenRequiresThePoolFlag`.

---

### JN-POOL-008: A stopped runner's steward falls back to the pool

**Preconditions:** `bob` started a runner, which registered, and then stopped
it. alice runs a pool runner.

**Steps:**

1. Wait at least three minutes after bob's runner stopped.
2. Run `schmeide status` as bob.
3. Assign an issue to `bob` and add `schmeide:ready`.

**Expected result:**

- Step 2 no longer lists bob's runner.
- Step 3: the job is published on `schmeide.jobs._pool.<repo id>.<issue>`,
  and alice's pool runner takes it.
- An issue made ready within three minutes of the stop still goes to bob's
  queue and waits for his next runner.
- A control node restart does not bring the registration back: its age comes
  from when it was stored.

**Notes:**

- The control node logs nothing when a registration expires. It logs
  `msg="runner registered"` for every registration it stores, each refresh
  included.
- Requires runners that re-publish their registration every
  `control.RegistrationInterval`; a runner that registers only at startup
  falls out of routing three minutes after it starts.
- Covered by `internal/control` `TestRegistrationExpiresWithoutRefresh`,
  `TestExpiredRegistrationSendsTheNextJobToThePool`, and
  `TestRestartKeepsOnlyLiveRegistrations`.
