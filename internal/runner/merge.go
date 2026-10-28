package runner

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/autobutler-org/schmeide/internal/ciwatch"
	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/forge"
)

// ciFixLead is the first sentence of a CI fix brief, so a fake harness can
// tell that session apart.
const ciFixLead = "A check failed on the open pull request for this change."

// ciFixBrief asks a worker session to fix the failed check run. tail is the
// end of that check's log.
func ciFixBrief(job control.Job, gate string, run ciwatch.Run, tail string) string {
	return fmt.Sprintf(`%[1]s You are working on GitHub issue #%[2]d of %[3]s.

Issue title: %[4]s

Issue body:
%[5]s

Check run %[6]q finished with %[7]s. The end of its log:
%[8]s

Fix what made that check fail, with tests where the repository expects them. Follow the repository's own conventions. Stay inside this directory. Do not push and do not open a pull request: the open pull request updates when the gate passes and this branch is pushed. The gate command is: %[9]s
`, ciFixLead, job.Issue, job.Repo, job.Title, job.Body, checkName(run), run.Conclusion, tail, gate)
}

// ciFix is one fix round for a failed check. It returns a final outcome, or
// stop false once the fix is pushed to the open pull request.
type ciFix func(ctx context.Context, run ciwatch.Run, tail string) (out outcome, stop bool)

// fixCheck runs one worker session for the failed check, the gate, and a
// push of the same branch. The commit uses the review fix subject, so a
// redelivery counts the round as spent.
func (r *Runner) fixCheck(ctx context.Context, ws Workspace, job control.Job, gate, prURL string, limits deadlines, run ciwatch.Run, tail string) (outcome, bool) {
	fail := func(err error) (outcome, bool) {
		return mergeFailed(prURL, fmt.Errorf("check run %s finished with %s, and the fix round failed: %w", checkName(run), run.Conclusion, err)), true
	}
	if err := finishSessionErr(r.harness.Name(), r.runSession(ctx, limits.session, r.session(ws.Dir(), ciFixBrief(job, gate, run, tail)))); err != nil {
		return fail(err)
	}
	dropSessionNotes(ws.Dir())
	changed, err := ws.Commit(ctx, fixMessage(job.Issue))
	if err != nil {
		return fail(err)
	}
	if !changed {
		return fail(fmt.Errorf("the %s session made no changes", r.harness.Name()))
	}
	if output, err := r.runGate(ctx, ws, limits.gate, gate); err != nil {
		return r.gateOutcome(job, gate, prURL, err, output), true
	}
	if err := ws.Push(ctx); err != nil {
		return fail(err)
	}
	return outcome{}, false
}

// logTail is the capped end of the failed run's log, or why it could not be
// read: a check that Actions did not run has no job log.
func logTail(ctx context.Context, gh forge.Forge, run ciwatch.Run) string {
	log, err := gh.JobLog(ctx, run.ID)
	if err != nil {
		return "(schmeide could not read the log: " + ciwatch.Tail([]byte(err.Error())) + ")"
	}
	return ciwatch.Tail(log)
}

// checkPoll is the first pause between reads of check runs. Each pause
// doubles, up to checkPollMax, so a slow CI is not read every second.
var (
	checkPoll    = time.Second
	checkPollMax = 30 * time.Second
)

type checkVerdict int

const (
	checksPending checkVerdict = iota
	checksGreen
	checksRed
)

// judgeChecks reports whether the head commit's check runs are green. No
// check runs is not green: that stays pending and the job keeps polling. A
// completed run whose conclusion is success or skipped counts as green.
// GitHub reports a path-filtered job, and a matrix job that did not run, as
// completed with conclusion skipped. Any other completed conclusion is red,
// including failure, cancelled, timed_out, and an empty conclusion. A run
// that has not completed stays pending so a later poll can see it finish.
func judgeChecks(runs []ciwatch.Run) (checkVerdict, string) {
	if len(runs) == 0 {
		return checksPending, "the head commit has no check runs, so CI is not green"
	}
	pending := ""
	for _, run := range runs {
		if run.Status != "completed" {
			if pending == "" {
				state := run.Status
				if state == "" {
					state = "unfinished"
				}
				pending = "check run " + checkName(run) + " is still " + state + ", so CI is not green"
			}
			continue
		}
		if run.Conclusion != "success" && run.Conclusion != "skipped" {
			conclusion := run.Conclusion
			if conclusion == "" {
				conclusion = "no conclusion"
			}
			return checksRed, "check run " + checkName(run) + " finished with " + conclusion + ", so CI is not green"
		}
	}
	if pending != "" {
		return checksPending, pending
	}
	return checksGreen, ""
}

func checkName(run ciwatch.Run) string {
	if run.Name == "" {
		return "unnamed"
	}
	return run.Name
}

func merged(prURL string) outcome {
	return outcome{
		state:   StateMerged,
		detail:  prURL,
		comment: "schmeide merged the pull request after the review passed and CI succeeded: " + prURL,
	}
}

// queueMerged is the outcome once a merge queue has merged the pull request.
// Adding it to the queue is not the end of the job.
func queueMerged(prURL string) outcome {
	return outcome{
		state:   StateMerged,
		detail:  prURL,
		comment: "the merge queue merged the pull request after the review passed and CI succeeded: " + prURL,
	}
}

// pullSettled reports a pull request that has merged, or that closed without
// merging. viaQueue is set once this job added it to a merge queue, and
// selects that comment. An open pull request is not settled. A missing pull
// request is not settled either: the list can lag behind the one just opened.
func pullSettled(ctx context.Context, gh forge.Forge, issue int, prURL string, viaQueue bool) (outcome, bool) {
	p, ok, err := gh.IssuePull(ctx, issue, "all")
	if err != nil {
		if ctx.Err() != nil {
			return outcome{}, true
		}
		return mergeFailed(prURL, fmt.Errorf("could not read the pull request for issue #%d: %w", issue, err)), true
	}
	if !ok {
		return outcome{}, false
	}
	if p.MergedAt != "" {
		if viaQueue {
			return queueMerged(prURL), true
		}
		return merged(prURL), true
	}
	if p.State == "closed" {
		return mergeFailed(prURL, errors.New("the pull request closed without merging, so schmeide stopped waiting")), true
	}
	return outcome{}, false
}

func mergeFailed(prURL string, err error) outcome {
	return outcome{
		state:   StateMergeFailed,
		detail:  err.Error(),
		comment: "schmeide failed this job and left the pull request open: " + prURL + "\n\n" + err.Error(),
	}
}

// mergeBlocked says why protection on base makes a factory merge impossible,
// or nil when it does not. Required approving reviews are the one such
// rule: the runner cannot approve its own pull request. Required status
// checks are not: the check wait covers them.
func mergeBlocked(base string, p forge.Protection) error {
	if p.RequiredApprovals > 0 {
		return fmt.Errorf("branch protection on %s requires %d approving review(s), and schmeide cannot supply that review, so it did not merge; a person can approve and merge the pull request", base, p.RequiredApprovals)
	}
	return nil
}

// mergeWhenGreen polls check runs on the pull request's head commit and
// merges with a merge commit when every completed run succeeded or was
// skipped. When the base branch has a merge queue, it adds the pull request
// to the queue and keeps watching until that queue merges it. The job stays
// in progress until the pull request has merged. A red check, a head commit
// with no check runs, and a merge GitHub refuses do not finish it. Polls
// back off from checkPoll to checkPollMax. The first failed check runs fix
// once, when fix is not nil, with the end of that check's log, and polling
// goes on at the new head. There is no second fix round. A fix round that
// itself fails is final, and so is a pull request that closes without merging.
//
// A pull request stacked on another one, whose base is not the default
// branch, merges only after the one below it: it first waits, for up to
// check_wait, until the one below merges, restacking as that branch moves,
// and is then retargeted to the default branch and rebased onto it; see
// [waitForStack]. Merging it into the branch below would leave its change off
// the default branch. check_wait bounds that stack wait. It does not end the
// wait for this pull request's own checks.
//
// Before the check wait, branch protection on the base is read, and a merge
// it makes impossible fails without waiting. A failure to read the merge
// queue, or to add the pull request to it, is final and says the checks were
// green.
func (r *Runner) mergeWhenGreen(ctx context.Context, ws Workspace, gh forge.Forge, issue int, base, prURL string, wait time.Duration, fix ciFix) outcome {
	trunk, err := gh.DefaultBranch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{}
		}
		return mergeFailed(prURL, fmt.Errorf("could not read the default branch: %w", err))
	}
	if base != trunk {
		if err := waitForStack(ctx, ws, gh, issue, trunk, wait); err != nil {
			if ctx.Err() != nil {
				return outcome{}
			}
			return mergeFailed(prURL, err)
		}
		base = trunk
	}
	protection, err := gh.Protection(ctx, base)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{}
		}
		return mergeFailed(prURL, fmt.Errorf("could not read branch protection on %s: %w", base, err))
	}
	if err := mergeBlocked(base, protection); err != nil {
		return mergeFailed(prURL, err)
	}
	sha, err := ws.Head(ctx)
	if err != nil {
		return mergeFailed(prURL, err)
	}
	number, err := pullNumber(prURL)
	if err != nil {
		return mergeFailed(prURL, err)
	}
	delay := checkPoll
	enqueued := false
	for {
		if ctx.Err() != nil {
			return outcome{}
		}
		if out, done := pullSettled(ctx, gh, issue, prURL, enqueued); done {
			return out
		}
		if enqueued {
			if !sleepFor(ctx, delay) {
				return outcome{}
			}
			delay = min(2*delay, checkPollMax)
			continue
		}
		runs, err := gh.CheckRuns(ctx, sha)
		if err != nil {
			if ctx.Err() != nil {
				return outcome{}
			}
			return mergeFailed(prURL, fmt.Errorf("could not read check runs for %s: %w", sha, err))
		}
		switch verdict, _ := judgeChecks(runs); verdict {
		case checksGreen:
			if ctx.Err() != nil {
				return outcome{}
			}
			queued, err := gh.QueuedPull(ctx, number)
			if err != nil {
				if ctx.Err() != nil {
					return outcome{}
				}
				return mergeFailed(prURL, fmt.Errorf("CI is green, but schmeide could not read the merge queue: %w", err))
			}
			if queued != "" {
				if err := gh.EnqueuePull(ctx, queued); err != nil {
					if ctx.Err() != nil {
						return outcome{}
					}
					return mergeFailed(prURL, fmt.Errorf("CI is green, but schmeide could not add the pull request to the merge queue: %w", err))
				}
				enqueued = true
				continue
			}
			if err := gh.MergePull(ctx, number); err != nil {
				if ctx.Err() != nil {
					return outcome{}
				}
				// The pull request is still open. Try again on the next poll.
				break
			}
			return merged(prURL)
		case checksRed:
			run, ok := ciwatch.NeedsTail(runs, fix == nil)
			if !ok {
				break
			}
			out, stop := fix(ctx, run, logTail(ctx, gh, run))
			fix = nil
			if ctx.Err() != nil {
				return outcome{}
			}
			if stop {
				return out
			}
			if sha, err = ws.Head(ctx); err != nil {
				return mergeFailed(prURL, err)
			}
			// The fix pushed a new head: its checks start over.
			delay = checkPoll
			continue
		}
		if !sleepFor(ctx, delay) {
			return outcome{}
		}
		delay = min(2*delay, checkPollMax)
	}
}

// waitForStack waits, backing off like the check wait, until the open pull
// request for issue targets trunk instead of the branch of the pull request
// below it, and restacks it on the way. While the pull request below
// is open, each poll rebases onto that branch, so the stack follows a base
// that moved. Once the pull request below merged, this one is retargeted to
// trunk, when GitHub did not already do that on deleting the merged branch,
// and rebased onto trunk. It returns why that did not happen within wait:
// the pull request below has not merged, it closed without merging, which
// only a person can sort out, or the restack failed.
func waitForStack(ctx context.Context, ws Workspace, gh forge.Forge, issue int, trunk string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	delay := checkPoll
	for {
		mine, ok, err := gh.IssuePull(ctx, issue, "open")
		if err != nil {
			return fmt.Errorf("could not read the pull request for issue #%d: %w", issue, err)
		}
		if !ok {
			return fmt.Errorf("the pull request for issue #%d is no longer open", issue)
		}
		if mine.Base.Ref == trunk {
			return restack(ctx, ws, trunk)
		}
		base := mine.Base.Ref
		below, err := stackedOn(base)
		if err != nil {
			return err
		}
		parent, ok, err := gh.IssuePull(ctx, below, "all")
		if err != nil {
			return fmt.Errorf("could not read the pull request for issue #%d, which this one is stacked on: %w", below, err)
		}
		if ok && parent.State == "open" {
			if err := restack(ctx, ws, base); err != nil {
				return err
			}
		} else {
			// The pull request says it merged even when it was rebased
			// first, which leaves the base this branch built on off trunk.
			merged := ok && parent.MergedAt != ""
			if !merged {
				number := 0
				if ok {
					number, _ = pullNumber(parent.HTMLURL)
				}
				if merged, err = ws.BaseMerged(ctx, trunk, number); err != nil {
					return fmt.Errorf("could not tell whether %s merged into %s: %w", base, trunk, err)
				}
			}
			if !merged {
				return fmt.Errorf("the pull request is stacked on %s, whose pull request for issue #%d closed without merging into %s, so schmeide did not merge it; a person can retarget it to %s", base, below, trunk, trunk)
			}
			if err := retarget(ctx, gh, mine.HTMLURL, trunk); err != nil {
				return err
			}
			return restack(ctx, ws, trunk)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the pull request is stacked on %s, which has not merged into %s after waiting %s (check_wait); schmeide merges a stack bottom first, so it did not merge this one", parent.HTMLURL, trunk, wait)
		}
		if !sleepUntil(ctx, delay, deadline) {
			return ctx.Err()
		}
		delay = min(2*delay, checkPollMax)
	}
}

// restack rebases the branch onto the remote branch onto, and says so when
// that fails.
func restack(ctx context.Context, ws Workspace, onto string) error {
	if _, err := ws.Restack(ctx, onto); err != nil {
		return fmt.Errorf("could not restack the pull request onto %s: %w", onto, err)
	}
	return nil
}

// retargeter is a forge that can change a pull request's base branch.
type retargeter interface {
	RetargetPull(ctx context.Context, number int, base string) error
}

// retarget points the pull request at prURL at base.
func retarget(ctx context.Context, gh forge.Forge, prURL, base string) error {
	rt, ok := gh.(retargeter)
	if !ok {
		return fmt.Errorf("the pull request below merged, but schmeide cannot retarget %s to %s on this forge; a person can retarget it", prURL, base)
	}
	number, err := pullNumber(prURL)
	if err != nil {
		return err
	}
	if err := rt.RetargetPull(ctx, number, base); err != nil {
		return fmt.Errorf("the pull request below merged, but schmeide could not retarget %s to %s: %w", prURL, base, err)
	}
	return nil
}

// stackedOn is the issue whose branch base is: schmeide names a job's branch
// "<issue>_<slug>".
func stackedOn(base string) (int, error) {
	n, _, ok := strings.Cut(base, "_")
	issue, err := strconv.Atoi(n)
	if !ok || err != nil || issue < 1 {
		return 0, fmt.Errorf("the pull request targets %s, which is neither the default branch nor a schmeide branch, so schmeide did not merge it", base)
	}
	return issue, nil
}

// sleepFor sleeps for delay. It returns false when ctx ends first.
func sleepFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// sleepUntil sleeps for delay, cut short at deadline so the last poll lands
// on it. It returns false when ctx ends first.
func sleepUntil(ctx context.Context, delay time.Duration, deadline time.Time) bool {
	t := time.NewTimer(max(min(delay, time.Until(deadline)), 0))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
