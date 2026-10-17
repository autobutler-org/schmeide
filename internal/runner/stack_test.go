package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// keep leaves a merged branch in place, so GitHub does not retarget the
	// pull requests on it.
	keep bool
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
	p.State, p.MergedAt = "closed", time.Now().UTC().Format(time.RFC3339)
	for i := range f.pulls {
		if !f.keep && f.pulls[i].Base.Ref == p.Head.Ref {
			f.pulls[i].Base.Ref = "main"
		}
	}
	f.merged = append(f.merged, number)
	return nil
}

func (f *stackForge) RetargetPull(_ context.Context, number int, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[number-1].Base.Ref = base
	return nil
}

func (f *stackForge) base(number int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[number-1].Base.Ref
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
	out := (&Runner{}).mergeWhenGreen(t.Context(), &stackWorkspace{}, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
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

// stackWorkspace is a headWorkspace that records restacks. merged is what
// BaseMerged reports, a non-nil restackErr fails every restack, and a
// non-nil restacked runs after each one.
type stackWorkspace struct {
	headWorkspace

	mu         sync.Mutex
	merged     bool
	restackErr error
	restacks   []string
	restacked  func()
}

func (w *stackWorkspace) Restack(_ context.Context, onto string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.restacks = append(w.restacks, onto)
	if w.restacked != nil {
		w.restacked()
	}
	return w.restackErr == nil, w.restackErr
}

func (w *stackWorkspace) BaseMerged(context.Context, string) (bool, error) { return w.merged, nil }

func (w *stackWorkspace) Restacks() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.restacks)
}

func TestMergeWhenGreenRetargetsAndRebasesAfterTheBottomMerges(t *testing.T) {
	// Pull 1 merged, but its branch was kept, so GitHub left pull 2 on it.
	gh := newStackForge(0, "success")
	gh.keep = true
	gh.pull(2, "1_x")
	gh.pulls[0].State = "closed"
	ws := &stackWorkspace{merged: true}
	out := (&Runner{}).mergeWhenGreen(t.Context(), ws, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
	if out.state != StateMerged {
		t.Fatalf("outcome = %+v, want merged", out)
	}
	if got := gh.base(2); got != "main" {
		t.Errorf("pull 2 targets %s, want main", got)
	}
	if got := ws.Restacks(); !slices.Equal(got, []string{"main"}) {
		t.Errorf("restacks = %v, want a rebase onto main", got)
	}
	if got := gh.Merged(); !slices.Equal(got, []int{2}) {
		t.Fatalf("merged %v, want [2]", got)
	}
}

func TestMergeWhenGreenRetargetsWhenTheBottomIsRebasedThenMerged(t *testing.T) {
	// Pull 1 is open on the first poll. Before the next one it is rebased
	// and merged, its branch kept, so the base pull 2 last built on is not
	// in main: only the pull request says it merged.
	gh := newStackForge(0, "success")
	gh.keep = true
	gh.pull(2, "1_x")
	ws := &stackWorkspace{}
	ws.restacked = func() {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		gh.pulls[0].State, gh.pulls[0].MergedAt = "closed", "2026-10-16T00:00:00Z"
	}
	out := (&Runner{}).mergeWhenGreen(t.Context(), ws, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
	if out.state != StateMerged {
		t.Fatalf("outcome = %+v, want merged", out)
	}
	if got := gh.base(2); got != "main" {
		t.Errorf("pull 2 targets %s, want main", got)
	}
	if got := ws.Restacks(); !slices.Equal(got, []string{"1_x", "main"}) {
		t.Errorf("restacks = %v, want a rebase onto 1_x, then onto main", got)
	}
	if got := gh.Merged(); !slices.Equal(got, []int{2}) {
		t.Fatalf("merged %v, want [2]", got)
	}
}

func TestMergeWhenGreenRebasesWhenGitHubRetargeted(t *testing.T) {
	gh := newStackForge(0, "success")
	gh.pull(2, "main")
	ws := &stackWorkspace{}
	out := (&Runner{}).mergeWhenGreen(t.Context(), ws, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
	if out.state != StateMerged {
		t.Fatalf("outcome = %+v, want merged", out)
	}
	if got := ws.Restacks(); !slices.Equal(got, []string{"main"}) {
		t.Errorf("restacks = %v, want a rebase onto main", got)
	}
}

func TestMergeWhenGreenFollowsAMovingBase(t *testing.T) {
	// Pull 1 stays open, so every poll rebases pull 2 onto its branch.
	gh := newStackForge(0, "success")
	gh.pull(2, "1_x")
	ws := &stackWorkspace{}
	out := (&Runner{}).mergeWhenGreen(t.Context(), ws, gh, 2, "1_x", gh.pulls[1].HTMLURL, 30*time.Millisecond, nil)
	if out.state != StateMergeFailed || !strings.Contains(out.detail, "has not merged") {
		t.Fatalf("outcome = %+v, want merge failed waiting on the bottom", out)
	}
	got := ws.Restacks()
	if len(got) == 0 || slices.ContainsFunc(got, func(b string) bool { return b != "1_x" }) {
		t.Errorf("restacks = %v, want rebases onto 1_x only", got)
	}
}

func TestMergeWhenGreenSaysWhenARestackConflicts(t *testing.T) {
	gh := newStackForge(0, "success")
	gh.keep = true
	gh.pull(2, "1_x")
	gh.pulls[0].State = "closed"
	ws := &stackWorkspace{merged: true, restackErr: errors.New("conflict in a.txt")}
	out := (&Runner{}).mergeWhenGreen(t.Context(), ws, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
	if out.state != StateMergeFailed || !strings.Contains(out.detail, "could not restack the pull request onto main: conflict in a.txt") {
		t.Fatalf("outcome = %+v, want merge failed naming the conflict", out)
	}
	if got := gh.Merged(); len(got) != 0 {
		t.Fatalf("merged %v, want nothing", got)
	}
}

func TestMergeWhenGreenRestacksARealBranchAfterTheBottomMerges(t *testing.T) {
	// main <- 1_x <- 2_x on a real remote. 1_x merges into main with a merge
	// commit, main moves on, and the branch is kept.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	src := t.TempDir()
	commit := func(name string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "add", "-A")
		git(t, src, "-c", "commit.gpgsign=false", "commit", "-q", "-m", name)
	}
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "config", "user.email", "dev@example.com")
	git(t, src, "config", "user.name", "Dev")
	commit("README")
	git(t, src, "checkout", "-q", "-b", "1_x")
	commit("one.txt")
	git(t, src, "checkout", "-q", "-b", "2_x")
	commit("two.txt")
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, src, "clone", "-q", "--bare", src, bare)
	git(t, src, "remote", "add", "origin", bare)

	ws, err := newWorktree(t.Context(), worktreeSpec{
		workDir: t.TempDir(), url: bare, base: "1_x", branch: "2_x",
		gitName: "schmeide[bot]", gitEmail: "bot@example.com", id: "2", repoID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err := ws.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}

	git(t, src, "checkout", "-q", "main")
	git(t, src, "-c", "commit.gpgsign=false", "merge", "-q", "--no-ff", "-m", "merge 1_x", "1_x")
	commit("later.txt")
	git(t, src, "push", "-q", "origin", "main")
	trunk := git(t, src, "rev-parse", "main")

	gh := newStackForge(0, "success")
	gh.keep = true
	gh.pull(2, "1_x")
	gh.pulls[0].State = "closed"
	out := (&Runner{}).mergeWhenGreen(t.Context(), ws, gh, 2, "1_x", gh.pulls[1].HTMLURL, time.Minute, nil)
	if out.state != StateMerged {
		t.Fatalf("outcome = %+v, want merged", out)
	}
	if got := gh.base(2); got != "main" {
		t.Errorf("pull 2 targets %s, want main", got)
	}
	if parent := git(t, bare, "rev-parse", "2_x^"); parent != trunk {
		t.Errorf("2_x's parent = %s, want main's head %s: the branch was not rebased", parent, trunk)
	}
	if subject := git(t, bare, "log", "-1", "--format=%s", "2_x"); subject != "two.txt" {
		t.Errorf("2_x's head = %q, want its own commit", subject)
	}
}

func TestBaseMergedTellsWhetherTheBaseIsOnTrunk(t *testing.T) {
	w, _ := newPushFixture(t)
	merged, err := w.BaseMerged(t.Context(), "main")
	if err != nil || !merged {
		t.Fatalf("BaseMerged = %v, %v; want true for a branch built on main", merged, err)
	}
	// A base that only a branch which closed unmerged holds.
	orphan := git(t, w.repoDir, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit-tree", "-m", "orphan", w.baseRef+"^{tree}")
	w.baseRef = "refs/remotes/origin/1_x"
	git(t, w.repoDir, "update-ref", w.baseRef, orphan)
	if merged, err = w.BaseMerged(t.Context(), "main"); err != nil || merged {
		t.Fatalf("BaseMerged = %v, %v; want false for a base that is not on main", merged, err)
	}
}

func TestGitHubRetargetPullPatchesTheBase(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.Path + " " + string(body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	gh := github{forge.GitHub{API: srv.URL, Repo: "o/r", Token: forge.StaticToken("t")}}
	if err := gh.RetargetPull(t.Context(), 2, "main"); err != nil {
		t.Fatal(err)
	}
	if want := `PATCH /repos/o/r/pulls/2 {"base":"main"}`; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
}
