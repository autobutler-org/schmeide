// Package forge is the code host a runner job talks to: pull requests,
// issue comments, check runs and their logs, merge, the merge queue, and
// branch protection. GitHub implements it over HTTP. Fake implements it in
// memory for tests.
package forge

import (
	"context"

	"github.com/autobutler-org/schmeide/internal/ciwatch"
)

// Forge is one repository on a code host, seen with one job's token.
type Forge interface {
	// DefaultBranch is the repository's default branch.
	DefaultBranch(ctx context.Context) (string, error)
	// BlockedBy lists the issues that block issue. No blockers is empty.
	BlockedBy(ctx context.Context, issue int) ([]Issue, error)
	// IssuePull returns the first pull request in state ("open", "closed",
	// or "all") whose head ref starts with "<issue>_". ok is false when
	// there is none.
	IssuePull(ctx context.Context, issue int, state string) (pull Pull, ok bool, err error)
	// OpenPull opens a pull request and returns its html URL.
	OpenPull(ctx context.Context, title, body, head, base string) (string, error)
	// Comment posts a comment on issue.
	Comment(ctx context.Context, issue int, body string) error
	// Comments lists the comments on issue since the RFC 3339 time since.
	// Empty since lists them all.
	Comments(ctx context.Context, issue int, since string) ([]Comment, error)
	// CheckRuns lists the latest check run for each name on sha.
	CheckRuns(ctx context.Context, sha string) ([]CheckRun, error)
	// JobLog reads the log of the Actions job behind check run id.
	JobLog(ctx context.Context, id int64) ([]byte, error)
	// QueuedPull is the node ID of pull request number when its base branch
	// has a merge queue, or "" when it does not.
	QueuedPull(ctx context.Context, number int) (string, error)
	// EnqueuePull adds the pull request with node ID id to its merge queue.
	EnqueuePull(ctx context.Context, id string) error
	// MergePull merges pull request number with a merge commit.
	MergePull(ctx context.Context, number int) error
	// Protection reads branch protection on branch. An unprotected branch
	// is the zero Protection and no error.
	Protection(ctx context.Context, branch string) (Protection, error)
}

// Pull is a pull request.
type Pull struct {
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	// MergedAt is when the pull request merged, empty when it has not. The
	// pull request list carries it; it does not carry "merged".
	MergedAt string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// Issue is an issue as the blocked-by list shows it.
type Issue struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// Comment is an issue comment.
type Comment struct {
	Body string `json:"body"`
	User struct {
		// Type is "Bot" for an app's comment.
		Type string `json:"type"`
	} `json:"user"`
}

// CheckRun is one check run on a commit.
type CheckRun = ciwatch.Run

// Protection is the part of branch protection the runner reads.
type Protection struct {
	// RequiredApprovals is how many approving reviews a pull request needs
	// before it can merge. Zero requires none.
	RequiredApprovals int
}
