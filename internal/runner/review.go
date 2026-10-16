package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
)

const (
	// reviewTextPath is where the reviewer leaves its verdict, relative to
	// the checkout. It is never committed.
	reviewTextPath = ".schmeide/review.md"

	// reviewLead is the first sentence of a review brief. A worker brief
	// never starts this way, so a fake harness can tell the sessions apart.
	reviewLead = "You are reviewing the change for a pull request that is already open."
)

type reviewVerdict int

const (
	verdictPass reviewVerdict = iota
	verdictChanges
)

// reviewBrief is the fixed prompt for one reviewer session. The verdict
// contract is the file at reviewTextPath: a fake harness passes, requests
// changes, or fails without a second harness and without parsing session
// output, which callers must not parse.
func reviewBrief(job control.Job) string {
	return fmt.Sprintf(`%[1]s

GitHub issue #%[2]d of %[3]s:
Title: %[4]s

%[5]s

Review the change in this checkout against the issue. Do not edit the code, do not commit, and do not push. Write your verdict to %[6]s and write nothing else.
- If the change does what the issue asks, the file's only line is: pass
- If it does not, the first line is "changes" and the rest of the file is what the worker must fix.

The first line must be "pass" or "changes".
`, reviewLead, job.Issue, job.Repo, job.Title, job.Body, reviewTextPath)
}

// fixBrief is the prompt for a fresh worker session that applies one round of
// review findings.
func fixBrief(job control.Job, gate, findings string) string {
	return fmt.Sprintf(`You are working on GitHub issue #%[1]d of %[2]s. A reviewer asked for changes on the open pull request.

Issue title: %[3]s

Issue body:
%[4]s

Reviewer findings:
%[5]s

Apply those findings, with tests where the repository expects them. Follow the repository's own conventions. Stay inside this directory. Do not push and do not open a pull request: the open pull request updates when the gate passes and this branch is pushed. The gate command is: %[6]s
`, job.Issue, job.Repo, job.Title, job.Body, findings, gate)
}

// readVerdict reads and removes the reviewer's verdict file.
func readVerdict(dir string) (reviewVerdict, string, error) {
	file := filepath.Join(dir, filepath.FromSlash(reviewTextPath))
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, "", fmt.Errorf("the reviewer did not write %s with a first line of \"pass\" or \"changes\": %w", reviewTextPath, err)
	}
	// Drop it before anything is committed. A pass leaves the tree untouched,
	// and a fix commit must not record the verdict.
	_ = os.Remove(file)
	line, rest, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	line, rest = strings.TrimSpace(line), strings.TrimSpace(rest)
	switch line {
	case "pass":
		if rest != "" {
			return 0, "", fmt.Errorf("the reviewer wrote a pass verdict with extra text in %s; a pass is the single line \"pass\"", reviewTextPath)
		}
		return verdictPass, "", nil
	case "changes":
		if rest == "" {
			return 0, "", errors.New("the reviewer requested changes but did not say what to fix")
		}
		return verdictChanges, rest, nil
	default:
		return 0, "", fmt.Errorf("the reviewer wrote %q as its verdict; the first line of %s must be \"pass\" or \"changes\"", line, reviewTextPath)
	}
}

// dropSessionNotes removes the files a session uses to talk to the runner, so
// a later commit does not record them.
func dropSessionNotes(dir string) {
	_ = os.Remove(filepath.Join(dir, filepath.FromSlash(prTextPath)))
	_ = os.Remove(filepath.Join(dir, filepath.FromSlash(reviewTextPath)))
}

// reviewFailed is a final failure after the pull request exists. It is not
// retried: the rounds it spent stay spent.
func reviewFailed(prURL string, err error) outcome {
	return outcome{
		state:   StateReviewFailed,
		detail:  err.Error(),
		comment: "schmeide failed this job and left the pull request open: " + prURL + "\n\n" + err.Error(),
	}
}

func passedReview(prURL string) outcome {
	return outcome{
		state:   StatePROpened,
		detail:  prURL,
		comment: "schmeide opened a pull request for this issue: " + prURL + "\n\nThe review passed.",
	}
}

func roundsExhausted(rounds int, prURL string) outcome {
	return outcome{
		state:   StateReviewFailed,
		detail:  fmt.Sprintf("the review did not pass within %d rounds", rounds),
		comment: fmt.Sprintf("schmeide failed this job because the review did not pass within %d rounds and left the pull request open: %s", rounds, prURL),
	}
}

// fixMessage is the subject of a commit that applies review findings. The
// commits with it on the branch are the rounds already spent.
func fixMessage(issue int) string {
	return fmt.Sprintf("Apply review feedback for #%d", issue)
}

// review runs reviewer sessions in the job's existing worktree until one
// passes or the round limit is hit. spent rounds already ran on this pull
// request before a redelivery. A pass returns an opened pull request. The
// caller merges only when the repo config says to.
func (r *Runner) review(ctx context.Context, ws Workspace, job control.Job, gate, prURL string, spent int, limits deadlines) outcome {
	rounds := r.opts.MaxReviewRounds
	if rounds <= 0 {
		rounds = defaultMaxReviewRounds
	}
	for round := spent + 1; round <= rounds; round++ {
		r.report(job, StateReviewing, fmt.Sprintf("round %d of %d", round, rounds))
		verdict, findings, err := r.reviewOnce(ctx, ws, job, limits.session)
		if err != nil {
			return reviewFailed(prURL, err)
		}
		if verdict == verdictPass {
			return passedReview(prURL)
		}
		// The last rejection is the result. Another fix would not be reviewed.
		if round == rounds {
			return roundsExhausted(rounds, prURL)
		}
		if out, stop := r.applyFindings(ctx, ws, job, gate, prURL, findings, limits); stop {
			return out
		}
	}
	return roundsExhausted(rounds, prURL)
}

// reviewOnce runs one fresh reviewer session and reads its verdict.
func (r *Runner) reviewOnce(ctx context.Context, ws Workspace, job control.Job, sessionLimit time.Duration) (reviewVerdict, string, error) {
	if err := finishSessionErr(r.harness.Name()+" reviewer", r.runSession(ctx, sessionLimit, r.session(ws.Dir(), reviewBrief(job)))); err != nil {
		return 0, "", err
	}
	return readVerdict(ws.Dir())
}

// applyFindings runs one fresh worker session, the gate, and a push of the
// same branch. stop is false when the open pull request now has the fix and
// another review round should run.
func (r *Runner) applyFindings(ctx context.Context, ws Workspace, job control.Job, gate, prURL, findings string, limits deadlines) (outcome, bool) {
	r.report(job, StateWorking, r.harness.Name())
	if err := finishSessionErr(r.harness.Name(), r.runSession(ctx, limits.session, r.session(ws.Dir(), fixBrief(job, gate, findings)))); err != nil {
		return reviewFailed(prURL, err), true
	}
	dropSessionNotes(ws.Dir())
	changed, err := ws.Commit(ctx, fixMessage(job.Issue))
	if err != nil {
		return reviewFailed(prURL, err), true
	}
	if !changed {
		return reviewFailed(prURL, fmt.Errorf("the %s session made no changes for the review findings", r.harness.Name())), true
	}
	output, err := r.runGate(ctx, ws, limits.gate, gate)
	if err != nil {
		// The fix stays unpushed, and the review does not continue.
		return r.gateOutcome(job, gate, prURL, err, output), true
	}
	if err := ws.Push(ctx); err != nil {
		return reviewFailed(prURL, err), true
	}
	return outcome{}, false
}

// session is one fresh harness session in the job's worktree.
func (r *Runner) session(dir, prompt string) harness.Request {
	return harness.Request{
		Dir:    dir,
		Prompt: prompt,
		Env:    harnessEnv(os.Environ(), r.opts.PassEnv),
		Output: r.opts.Output,
	}
}
