# Job flows

The paths below cover what the factory does at runtime. Each step names the package or file that owns it, so a
diagram doubles as a map for where to start reading. Policy (what is retried, what is final, and why) is in
[FAILURE-MODES.md](../FAILURE-MODES.md). This page shows the mechanism.

## a. Labeled issue to opened pull request

```mermaid
sequenceDiagram
    autonumber
    actor P as Person
    participant GH as GitHub
    participant H as control.Handler<br/>+ Intake
    participant Q as control.Queue
    participant KV as SCHMEIDE_ISSUES
    participant JS as SCHMEIDE_JOBS
    participant R as runner.handle / work
    participant W as runner worktree
    participant X as harness (claude)

    P->>GH: assign issue, add schmeide:ready
    GH->>H: POST /webhook (issues, labeled)
    H->>H: HMAC, rate limit, replay check (intake.go, guard.go)
    H->>H: JobFromIssuesEvent, Allowlist.allows
    H->>Q: Publish(job)
    Q->>Q: Tracker.hasRunner(steward)? steward subject : pool subject
    Q->>KV: Create {repo_id}.{issue} with After = last status seq
    Q->>Q: status pending (SCHMEIDE_STATUS, publishPending)
    Q->>JS: publish schmeide.jobs.{steward}.{repo_id}.{issue}
    H-->>GH: 202 created
    JS->>R: Next() on consumer runner-{steward}
    R->>Q: status claimed (SCHMEIDE_STATUS)
    R->>Q: request schmeide.token.{steward} with job subject
    Q->>GH: POST /app/installations/{id}/access_tokens (token.go)
    GH-->>Q: installation token, 1 hour
    Q-->>R: TokenReply
    R->>R: trust.Check(work dir, origin)
    R->>GH: IssuePull(issue, all) finds no PR (forge)
    R->>GH: DefaultBranch, BlockedBy (github.go baseBranch)
    R->>W: newWorktree: fetch base, worktree add, refguard.Install
    R->>Q: stage worktree_ready (SCHMEIDE_STAGES)
    R->>W: repoconfig.Load, lintgate.Detect
    R->>Q: status working
    R->>X: Run(brief + repomap), session env, session_timeout
    X->>W: edits files, writes .schmeide/pr.md
    R->>Q: stage session_finished
    R->>W: prText, Commit
    R->>W: Run(gate), gate_timeout
    R->>Q: stage gate_passed
    R->>W: Push: secretscan.Scan, ls-remote, git push
    W->>GH: push {issue}_{slug}
    R->>GH: OpenPull(title, body, branch, base)
    R->>Q: stage pr_opened (PR URL, opened time)
    Note over R: review runs next, see b
```

## b. Review rounds

### With `automate.review_fix` off

```mermaid
sequenceDiagram
    autonumber
    participant R as runner.review
    participant Q as control node
    participant V as reviewer harness<br/>(runner.reviewer)
    participant W as worktree
    participant GH as GitHub

    R->>Q: status reviewing (round 1 of 3), stage review round 1
    R->>V: Run(reviewBrief + repomap)
    V->>W: write .schmeide/review.md
    R->>W: readVerdict, delete review.md
    alt verdict pass
        R->>GH: comment "opened a pull request ... The review passed." + outcome marker
        R->>Q: status pr_opened, delete stage, Ack
    else verdict changes
        R->>GH: comment "left the pull request open for a person"
        R->>Q: status review_failed, delete stage, Ack
    end
```

### With `automate.review_fix = "on"`

```mermaid
sequenceDiagram
    autonumber
    participant R as runner.review
    participant Q as control node
    participant V as reviewer harness
    participant X as writer harness
    participant W as worktree
    participant GH as GitHub

    loop round = spent+1 .. --max-review-rounds
        R->>Q: status reviewing (round N of M), stage review round N
        R->>V: Run(reviewBrief)
        R->>W: readVerdict
        alt pass
            R->>GH: comment, status pr_opened (or factory merge, see c)
        else changes in the last round
            R->>GH: comment rounds exhausted, status review_failed
        else changes, rounds left
            R->>Q: status working
            R->>X: Run(fixBrief with findings)
            R->>W: dropSessionNotes, Commit "Apply review feedback for N"
            R->>W: Run(gate)
            alt gate fails
                R->>GH: comment "left the pull request open", status gate_failed
            else gate passes
                R->>W: Push same branch (secret scan, fast-forward only)
                W->>GH: open PR updates
            end
        end
    end
```

A reviewer file that is missing, has extra text after `pass`, or has `changes` with no findings ends the job as
`review_failed` (`readVerdict` in `internal/runner/review.go`).

## c. Factory merge

```mermaid
sequenceDiagram
    autonumber
    participant R as runner.mergeWhenGreen
    participant F as forge.GitHub
    participant W as worktree
    participant X as writer harness

    R->>F: DefaultBranch
    opt PR base is not the default branch (stacked)
        loop waitForStack, backoff 1s to 30s, up to check_wait
            R->>F: IssuePull(issue, open), IssuePull(below, all)
            alt PR below still open
                R->>W: Restack onto its branch, force-with-lease
            else PR below merged
                R->>F: RetargetPull to default branch (PATCH /pulls/N)
                R->>W: Restack onto default branch
            end
        end
    end
    R->>F: Protection(base)
    alt required approving reviews above 0
        R-->>R: merge_failed, no wait
    end
    loop poll check runs on head, backoff 1s to 30s, until check_wait
        R->>F: CheckRuns(sha)
        alt all completed and success
            R->>F: QueuedPull(number) via GraphQL
            alt base branch has a merge queue
                R->>F: EnqueuePull(node id) via GraphQL
                R-->>R: queued
            else no merge queue
                R->>F: MergePull(number), merge_method merge
                R-->>R: merged
            end
        else first red check, a review round left
            R->>F: JobLog(check run id), ciwatch.Tail
            R->>X: Run(ciFixBrief with log tail)
            R->>W: Commit "Apply review feedback for N", gate, Push
            Note over R: poll again on the new head, same deadline
        else red again, or no round left
            R-->>R: merge_failed
        else pending or no check runs
            Note over R: sleep, then poll again
        end
    end
    Note over R: deadline reached without green: merge_failed
```

## d. A runner killed mid-job

The e2e journey `TestJourneyKilledRunnerJobIsRedeliveredWithoutASecondPullRequest` in `internal/e2e` drives this
path with a real binary.

```mermaid
sequenceDiagram
    autonumber
    participant JS as SCHMEIDE_JOBS<br/>consumer runner-{steward}
    participant R1 as runner 1
    participant KV as SCHMEIDE_STAGES
    participant ST as SCHMEIDE_STATUS
    participant R2 as runner 2
    participant GH as GitHub

    JS->>R1: deliver job
    loop every ack-wait / 3
        R1->>JS: InProgress
    end
    R1->>GH: push branch, open PR
    R1->>KV: stage pr_opened (branch, base, PR URL, opened)
    R1->>KV: stage review round 1
    Note over R1: process killed, no Ack, heartbeats stop
    Note over JS: ack-wait passes with no InProgress
    JS->>R2: redeliver job
    R2->>ST: GetLastMsgForSubject(status subject) restores attempts
    R2->>GH: IssuePull(issue, all) finds the open PR on {issue}_
    R2->>KV: recordedStage (fallback when GitHub lists no PR)
    R2->>GH: Comments since PR creation, look for outcome marker
    alt outcome marker found
        R2->>ST: report that state again, Ack, no new comment
    else PR closed
        R2->>ST: pr_opened, Ack
    else PR open
        R2->>R2: newWorktree on the PR head, Resume (reset to remote head)
        R2->>R2: Count "Apply review feedback" commits, max with stage round - 1
        R2->>R2: review from the next round, no second PR
    end
```

The same path covers a control node restart while a runner holds a job: the runner keeps reconnecting
(`nats.MaxReconnects(-1)`), and if the lease lapses the job is redelivered and resumes the same way.

## e. Pool fallback

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub
    participant Q as control.Queue
    participant T as control.Tracker
    participant JS as SCHMEIDE_JOBS
    participant PR as pool runner (steward b, --pool)

    GH->>Q: Publish(job for steward a)
    Q->>T: hasRunner(a)
    T-->>Q: false (no registration from a ever)
    Q->>JS: publish schmeide.jobs._pool.{repo_id}.{issue} with pool true
    loop runner.next
        PR->>JS: Next() on runner-b, timeout
        PR->>JS: Next() on runner-_pool
    end
    JS->>PR: deliver pool job
    PR->>Q: status on schmeide.status._pool...
    PR->>Q: request schmeide.token.b with the pool subject
    Q->>Q: tokenFor: job.Pool, Tracker.poolRunner(b)
    Q->>GH: mint token for the job's repository
    Q-->>PR: TokenReply
    Note over PR: same worktree, gate, review, and merge path as a,<br/>stage key _pool.{repo_id}.{issue}
```

A pool job matches no steward's filter, because `_pool` is refused as a steward name at intake and in
`--runner-tokens`. A runner without `--pool` never creates the pool consumer. Once a steward has registered a
runner, that steward's later jobs go to the steward subject even when every one of its runners has stopped.

## f. Retries, timeouts, and the stuck-job drop

```mermaid
sequenceDiagram
    autonumber
    participant JS as JetStream delivery
    participant R as runner.handle
    participant WD as heartbeat / watchdog goroutine
    participant ST as SCHMEIDE_STATUS
    participant GH as GitHub issue

    JS->>R: deliver
    R->>ST: recordedAttempts from last status
    R->>WD: start (tick ack-wait/3, timer stuck-after)
    alt token request fails
        R->>JS: NakWithDelay 30s, no attempt counted
    else stuck-after passes with no report
        WD->>R: cancel work (errJobStuck), sessions and gate killed
        WD->>JS: Nak, no attempt counted, no comment
    else runner shutting down
        R->>JS: Nak, no attempt counted
    else work ends in failed (session error or timeout, git, push, GitHub before the PR)
        R->>R: attempts + 1
        alt attempts below max-attempts
            R->>ST: status retrying with attempts
            R->>JS: NakWithDelay 30s
        else attempts reached
            R->>GH: comment with outcome marker
            R->>ST: status failed
            R->>JS: Ack
        end
    else work ends in a final failed (repo config error, blockers not one linear stack)
        R->>GH: comment with outcome marker
        R->>ST: status failed
        R->>JS: Ack, no retry
    else gate fails or times out
        R->>GH: comment without output
        R->>ST: status gate_failed
        R->>JS: Ack
    end
```

Deadlines are child contexts: `runSession` wraps each harness run in `session_timeout` (or `--session-timeout`),
and `runGate` wraps the gate in `gate_timeout` (or `--gate-timeout`). Both kill their process group on expiry,
and the resulting error is a `timeoutError` whose text carries no command output.
