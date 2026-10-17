package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/autobutler-org/schmeide/internal/forge"
)

// baseBranch is the branch a new pull request for issue is opened against,
// and the branch its worktree is created from. It is the repository default
// branch when no open blocker has an open schmeide pull request: there is no
// blocker, every blocker is already merged, or a blocker has no open pull
// request whose branch starts with that issue's "<number>_". Exactly one
// such pull request selects its branch. More than one is refused.
func baseBranch(ctx context.Context, gh forge.Forge, issue int) (string, error) {
	base, err := gh.DefaultBranch(ctx)
	if err != nil {
		return "", err
	}
	blockers, err := gh.BlockedBy(ctx, issue)
	if err != nil {
		return "", err
	}
	stacked := ""
	for _, blocker := range blockers {
		if blocker.State != "open" {
			continue
		}
		pr, ok, err := gh.IssuePull(ctx, blocker.Number, "open")
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		if stacked != "" {
			return "", errors.New("only one open dependency is supported")
		}
		stacked = pr.Head.Ref
	}
	if stacked != "" {
		return stacked, nil
	}
	return base, nil
}

// outcomeMarker tags the issue comment that carries a job's outcome, followed
// by the outcome's state and " -->". A redelivered job reads it back to tell
// that its outcome was already posted.
const outcomeMarker = "<!-- schmeide:outcome "

// markOutcome appends the outcome marker for state to an outcome comment.
func markOutcome(body string, state State) string {
	return body + "\n\n" + outcomeMarker + string(state) + " -->"
}

// postedOutcome returns the state in the last outcome comment a bot posted on
// the issue since the RFC 3339 time since, or false when there is none. Empty
// since reads every comment.
func postedOutcome(ctx context.Context, gh forge.Forge, issue int, since string) (State, bool, error) {
	comments, err := gh.Comments(ctx, issue, since)
	if err != nil {
		return "", false, err
	}
	var state State
	found := false
	for _, c := range comments {
		if c.User.Type != "Bot" {
			continue
		}
		_, rest, ok := strings.Cut(c.Body, outcomeMarker)
		if !ok {
			continue
		}
		if s, _, ok := strings.Cut(rest, " -->"); ok {
			state, found = State(s), true
		}
	}
	return state, found, nil
}

// pullNumber is the pull request number at the end of a GitHub html URL.
func pullNumber(prURL string) (int, error) {
	u, err := url.Parse(prURL)
	if err != nil {
		return 0, fmt.Errorf("pull request URL %q: %w", prURL, err)
	}
	n, err := strconv.Atoi(path.Base(u.Path))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("pull request URL %q has no pull number", prURL)
	}
	return n, nil
}
