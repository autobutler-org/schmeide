package forge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory Forge for tests. It makes no network calls. Set its
// exported fields before handing it to the code under test, and read what
// that code did with Comments, Merged, and Enqueued. The zero Fake is a
// repository on main with no pull requests, no check runs, no merge queue,
// and no branch protection.
type Fake struct {
	// Default is the default branch. Empty is "main".
	Default  string
	Blockers map[int][]Issue
	Pulls    []Pull
	// Checks are the check runs every commit has.
	Checks []CheckRun
	// Logs are the Actions job logs by job ID. A job without one is an error.
	Logs map[int64]string
	// QueueID, when set, is the node ID QueuedPull returns: the base branch
	// has a merge queue.
	QueueID string
	// Protections is the branch protection by branch.
	Protections map[string]Protection

	mu       sync.Mutex
	comments map[int][]stamped
	merged   []int
	enqueued []string
}

var _ Forge = (*Fake)(nil)

// stamped is a comment and when Comment stored it.
type stamped struct {
	Comment
	at time.Time
}

func (f *Fake) DefaultBranch(context.Context) (string, error) {
	if f.Default == "" {
		return "main", nil
	}
	return f.Default, nil
}

func (f *Fake) BlockedBy(_ context.Context, issue int) ([]Issue, error) {
	return slices.Clone(f.Blockers[issue]), nil
}

func (f *Fake) IssuePull(_ context.Context, issue int, state string) (Pull, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.Pulls {
		if strings.HasPrefix(p.Head.Ref, fmt.Sprintf("%d_", issue)) && (state == "all" || state == p.State) {
			return p, true, nil
		}
	}
	return Pull{}, false, nil
}

func (f *Fake) OpenPull(_ context.Context, _, _, head, base string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := Pull{HTMLURL: fmt.Sprintf("https://github.test/o/r/pull/%d", len(f.Pulls)+1), State: "open"}
	p.Head.Ref, p.Base.Ref = head, base
	f.Pulls = append(f.Pulls, p)
	return p.HTMLURL, nil
}

// Comment stores body as a bot's comment, stamped with the time now.
func (f *Fake) Comment(_ context.Context, issue int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[int][]stamped{}
	}
	c := stamped{Comment: Comment{Body: body}, at: time.Now()}
	c.User.Type = "Bot"
	f.comments[issue] = append(f.comments[issue], c)
	return nil
}

// Comments lists the comments stored at or after since, as GitHub does.
// Empty since lists them all.
func (f *Fake) Comments(_ context.Context, issue int, since string) ([]Comment, error) {
	var after time.Time
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return nil, fmt.Errorf("comments since %q: %w", since, err)
		}
		after = t
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []Comment
	for _, c := range f.comments[issue] {
		if !c.at.Before(after) {
			found = append(found, c.Comment)
		}
	}
	return found, nil
}

func (f *Fake) CheckRuns(context.Context, string) ([]CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Checks), nil
}

func (f *Fake) JobLog(_ context.Context, id int64) ([]byte, error) {
	log, ok := f.Logs[id]
	if !ok {
		return nil, fmt.Errorf("no log for job %d", id)
	}
	return []byte(log), nil
}

func (f *Fake) QueuedPull(context.Context, int) (string, error) { return f.QueueID, nil }

func (f *Fake) EnqueuePull(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, id)
	return nil
}

func (f *Fake) MergePull(_ context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged = append(f.merged, number)
	return nil
}

func (f *Fake) Protection(_ context.Context, branch string) (Protection, error) {
	return f.Protections[branch], nil
}

// Merged returns the pull request numbers merged so far.
func (f *Fake) Merged() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.merged)
}

// Enqueued returns the node IDs added to the merge queue so far.
func (f *Fake) Enqueued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.enqueued)
}
