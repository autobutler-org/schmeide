package runner

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// checkWait is how long factory merge polls the head commit's check runs.
// Tests shorten it.
var checkWait = 2 * time.Minute

// checkPoll is the pause between those reads.
const checkPoll = time.Second

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
func judgeChecks(runs []ghCheckRun) (checkVerdict, string) {
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

func checkName(run ghCheckRun) string {
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

func mergeFailed(prURL string, err error) outcome {
	return outcome{
		state:   StateMergeFailed,
		detail:  err.Error(),
		comment: "schmeide failed this job and left the pull request open: " + prURL + "\n\n" + err.Error(),
	}
}

// mergeWhenGreen polls check runs on the pull request's head commit and
// merges with a merge commit when every one has succeeded. A failed check,
// or a wait of checkWait that does not end in success, leaves the pull
// request open. That failure is final: the pull request already exists, and
// a redelivery would treat it as done.
func (r *Runner) mergeWhenGreen(ctx context.Context, ws Workspace, gh github, prURL string) outcome {
	sha, err := ws.Head(ctx)
	if err != nil {
		return mergeFailed(prURL, err)
	}
	number, err := pullNumber(prURL)
	if err != nil {
		return mergeFailed(prURL, err)
	}
	deadline := time.Now().Add(checkWait)
	detail := "the head commit has no check runs, so CI is not green"
	ticker := time.NewTicker(checkPoll)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return outcome{}
		}
		runs, err := gh.checkRuns(ctx, sha)
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
			if err := gh.mergePull(ctx, number); err != nil {
				if ctx.Err() != nil {
					return outcome{}
				}
				return mergeFailed(prURL, fmt.Errorf("could not merge the pull request: %w", err))
			}
			return merged(prURL)
		case checksRed:
			return mergeFailed(prURL, errors.New(why))
		}
		if !time.Now().Before(deadline) {
			return mergeFailed(prURL, errors.New(detail))
		}
		select {
		case <-ctx.Done():
			return outcome{}
		case <-ticker.C:
		}
	}
}
