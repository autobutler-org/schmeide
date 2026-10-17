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
// check runs is not green: that stays pending until the wait ends. A
// completed run whose conclusion is not success is red. A run that has not
// completed stays pending so a later poll can see it finish.
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
		if run.Conclusion != "success" {
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

// enqueued is the outcome when the base branch has a merge queue: the queue
// merges the pull request, so the job ends as queued.
func enqueued(prURL string) outcome {
	return outcome{
		state:   StateQueued,
		detail:  prURL,
		comment: "schmeide added the pull request to the merge queue after the review passed and CI succeeded: " + prURL,
	}
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
// merges with a merge commit when every one has succeeded. When the base
// branch has a merge queue, it adds the pull request to the queue instead.
// Polls back off from checkPoll to checkPollMax, and the last one lands on
// the end of the wait. The first failed check runs fix once, when fix is not
// nil, with the end of that check's log, and polling goes on at the new head
// within the same wait. A failed check after that, or a wait of the repo's
// check_wait that does not end in success, leaves the pull request open.
// That failure is final: the pull request already exists, and a redelivery
// would treat it as done.
//
// A pull request stacked on another one, whose base is not the default
// branch, merges only after the one below it: it first waits, for up to
// check_wait, until it targets the default branch, which GitHub does when the
// pull request below merges and its branch is deleted. Merging it into the
// branch below would leave its change off the default branch.
//
// Before the check wait, branch protection on the base is read, and a merge
// it makes impossible fails without waiting. Every failure after the checks
// are green says so, so a pull request that looks ready names why it was not
// merged.
func (r *Runner) mergeWhenGreen(ctx context.Context, ws Workspace, gh forge.Forge, issue int, base, prURL string, wait time.Duration, fix ciFix) outcome {
	trunk, err := gh.DefaultBranch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{}
		}
		return mergeFailed(prURL, fmt.Errorf("could not read the default branch: %w", err))
	}
	if base != trunk {
		if err := waitForStack(ctx, gh, issue, base, trunk, wait); err != nil {
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
	deadline := time.Now().Add(wait)
	detail := "the head commit has no check runs, so CI is not green"
	delay := checkPoll
	for {
		if ctx.Err() != nil {
			return outcome{}
		}
		runs, err := gh.CheckRuns(ctx, sha)
		if err != nil {
			if ctx.Err() != nil {
				return outcome{}
			}
			return mergeFailed(prURL, fmt.Errorf("could not read check runs for %s: %w", sha, err))
		}
		verdict, why := judgeChecks(runs)
		if why != "" {
			detail = why
		}
		switch verdict {
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
				return enqueued(prURL)
			}
			if err := gh.MergePull(ctx, number); err != nil {
				if ctx.Err() != nil {
					return outcome{}
				}
				return mergeFailed(prURL, fmt.Errorf("CI is green, but GitHub refused the merge: %w", err))
			}
			return merged(prURL)
		case checksRed:
			run, ok := ciwatch.NeedsTail(runs, fix == nil)
			if !ok {
				return mergeFailed(prURL, errors.New(why))
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
		if !time.Now().Before(deadline) {
			return mergeFailed(prURL, fmt.Errorf("%s after waiting %s (check_wait)", detail, wait))
		}
		if !sleepUntil(ctx, delay, deadline) {
			return outcome{}
		}
		delay = min(2*delay, checkPollMax)
	}
}

// waitForStack waits, backing off like the check wait, until the open pull
// request for issue targets trunk instead of base, the branch of the pull
// request below it. It returns why that did not happen within wait: the pull
// request below has not merged, or it closed and this one still targets its
// branch, which only a person can fix.
func waitForStack(ctx context.Context, gh forge.Forge, issue int, base, trunk string, wait time.Duration) error {
	below, err := stackedOn(base)
	if err != nil {
		return err
	}
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
			return nil
		}
		parent, ok, err := gh.IssuePull(ctx, below, "all")
		if err != nil {
			return fmt.Errorf("could not read the pull request for issue #%d, which this one is stacked on: %w", below, err)
		}
		if !ok || parent.State != "open" {
			return fmt.Errorf("the pull request is stacked on %s, whose pull request for issue #%d is no longer open, but it still targets %s and not %s, so schmeide did not merge it; a person can retarget it to %s", mine.Base.Ref, below, mine.Base.Ref, trunk, trunk)
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
