package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/forge"
)

// stackForge is a forge.Fake whose pull requests and check runs change as
// the code under test polls them. Pulls are numbered from 1 in order.
// Merging one closes it and, as GitHub does when the merged branch is
// deleted, retargets the pull requests on its branch to main. Checks for a
// pull request's head are pending for its first pending reads, then
// conclude with conclusion.
type stackForge struct {
	*forge.Fake

	mu         sync.Mutex
	pulls      []forge.Pull
	pending    int
	conclusion string
	reads      int
	merged     []int
}

// pull adds an open pull request for issue on head "<issue>_x" into base.
func (f *stackForge) pull(issue int, base string) {
	p := forge.Pull{HTMLURL: fmt.Sprintf("https://github.com/o/r/pull/%d", len(f.pulls)+1), State: "open"}
	p.Head.Ref, p.Base.Ref = fmt.Sprintf("%d_x", issue), base
	f.pulls = append(f.pulls, p)
}

func (f *stackForge) IssuePull(_ context.Context, issue int, state string) (forge.Pull, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pulls {
		if strings.HasPrefix(p.Head.Ref, fmt.Sprintf("%d_", issue)) && (state == "all" || state == p.State) {
			return p, true, nil
		}
	}
	return forge.Pull{}, false, nil
}

func (f *stackForge) CheckRuns(context.Context, string) ([]forge.CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.reads <= f.pending {
		return []forge.CheckRun{{Name: "ci", Status: "in_progress"}}, nil
	}
	return []forge.CheckRun{{Name: "ci", Status: "completed", Conclusion: f.conclusion}}, nil
}

func (f *stackForge) MergePull(_ context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &f.pulls[number-1]
	if p.Base.Ref != "main" {
		return fmt.Errorf("pull %d targets %s, not main", number, p.Base.Ref)
	}
	p.State = "closed"
	for i := range f.pulls {
		if f.pulls[i].Base.Ref == p.Head.Ref {
			f.pulls[i].Base.Ref = "main"
		}
	}
	f.merged = append(f.merged, number)
	return nil
}

func (f *stackForge) Merged() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.merged)
}

func newStackForge(pending int, conclusion string) *stackForge {
	f := &stackForge{Fake: &forge.Fake{}, pending: pending, conclusion: conclusion}
	f.pull(1, "main")
	return f
}

func TestMergeWhenGreenMergesSlowGreenChecks(t *testing.T) {
	// Twenty pending reads outlast a fixed poll; the backoff keeps polling
	// until the wait ends.
	gh := newStackForge(20, "success")
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, 1, "main", gh.pulls[0].HTMLURL, time.Minute, nil)
	if out.state != StateMerged {
		t.Fatalf("outcome = %+v, want merged", out)
	}
	if got := gh.Merged(); !slices.Equal(got, []int{1}) {
		t.Fatalf("merged %v, want [1]", got)
	}
}

func TestMergeWhenGreenDoesNotMergeRedChecks(t *testing.T) {
	gh := newStackForge(3, "failure")
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, 1, "main", gh.pulls[0].HTMLURL, time.Minute, nil)
	if out.state != StateMergeFailed || !strings.Contains(out.comment, "check run ci finished with failure") {
		t.Fatalf("outcome = %+v, want merge failed naming the red check", out)
	}
	if got := gh.Merged(); len(got) != 0 {
		t.Fatalf("merged %v, want nothing", got)
	}
}

func TestMergeWhenGreenSaysHowLongItWaited(t *testing.T) {
	gh := newStackForge(1<<30, "success")
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, 1, "main", gh.pulls[0].HTMLURL, 30*time.Millisecond, nil)
	if out.state != StateMergeFailed || !strings.Contains(out.detail, "still in_progress") || !strings.Contains(out.detail, "check_wait") {
		t.Fatalf("outcome = %+v, want merge failed naming the pending check and check_wait", out)
	}
}

func TestMergeWhenGreenMergesAStackBottomFirst(t *testing.T) {
	// Pull 2, for issue 2, is stacked on pull 1's branch. Both runners wait
	// at once; the top one starts first and its checks are green at once.
	gh := newStackForge(5, "success")
	gh.pull(2, "1_x")
	var wg sync.WaitGroup
	outs := make([]outcome, 2)
	for i, issue := range []int{2, 1} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := map[int]string{1: "main", 2: "1_x"}[issue]
			outs[i] = (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, issue, base, gh.pulls[issue-1].HTMLURL, time.Minute, nil)
		}()
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()
	for _, out := range outs {
		if out.state != StateMerged {
			t.Fatalf("outcome = %+v, want merged", out)
		}
	}
	if got := gh.Merged(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("merged %v, want the bottom first: [1 2]", got)
	}
}

func TestMergeWhenGreenWaitsForTheBottomOfAStack(t *testing.T) {
	gh := newStackForge(0, "success")
	gh.pull(2, "1_x")
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, 2, "1_x", gh.pulls[1].HTMLURL, 30*time.Millisecond, nil)
	if out.state != StateMergeFailed {
		t.Fatalf("outcome = %+v, want merge failed", out)
	}
	for _, want := range []string{"stacked on https://github.com/o/r/pull/1", "has not merged", "bottom first"} {
		if !strings.Contains(out.detail, want) {
			t.Errorf("detail = %q, want it to say %q", out.detail, want)
		}
	}
	if got := gh.Merged(); len(got) != 0 {
		t.Fatalf("merged %v, want nothing", got)
	}
}

func TestMergeWhenGreenRefusesAStackOnAClosedPull(t *testing.T) {
	// Pull 1 closed without merging, so GitHub did not retarget pull 2.
	gh := newStackForge(0, "success")
	gh.pull(2, "1_x")
	gh.pulls[0].State = "closed"
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
	if out.state != StateMergeFailed || !strings.Contains(out.detail, "retarget it to main") {
		t.Fatalf("outcome = %+v, want merge failed asking a person to retarget", out)
	}
	if got := gh.Merged(); len(got) != 0 {
		t.Fatalf("merged %v, want nothing", got)
	}
}

func TestMergeWhenGreenSaysWhyAGreenPullDidNotMerge(t *testing.T) {
	gh := &refusingForge{Fake: &forge.Fake{Checks: []forge.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}}}
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, 1, "main", "https://github.com/o/r/pull/9", time.Minute, nil)
	if out.state != StateMergeFailed || !strings.Contains(out.detail, "CI is green, but GitHub refused the merge: not mergeable") {
		t.Fatalf("outcome = %+v, want merge failed saying CI was green and why it did not merge", out)
	}
}

// refusingForge is a forge.Fake whose merges fail.
type refusingForge struct{ *forge.Fake }

func (refusingForge) MergePull(context.Context, int) error {
	return errors.New("not mergeable")
}
