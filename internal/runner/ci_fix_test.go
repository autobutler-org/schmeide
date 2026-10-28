package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/ciwatch"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/testutil/fakegh"
)

// ciFixHarness passes every review and writes fix.txt in the first worker
// session. A CI fix session records its prompt, writes ci.txt, and calls
// onFix. rejects is how many reviews ask for changes before one passes.
func ciFixHarness(rejects int32, onFix func()) (*harness.Fake, *atomic.Int32, *atomic.Value) {
	var fixes, reviews atomic.Int32
	var prompt atomic.Value
	return &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		file, content := "fix.txt", "fixed\n"
		switch {
		case strings.Contains(req.Prompt, reviewLead):
			if reviews.Add(1) <= rejects {
				return writeVerdict(req, "changes\nrename the thing\n")
			}
			return writeVerdict(req, "pass\n")
		case strings.Contains(req.Prompt, ciFixLead):
			fixes.Add(1)
			prompt.Store(req.Prompt)
			onFix()
			file = "ci.txt"
		case strings.Contains(req.Prompt, "Reviewer findings"):
			content = fmt.Sprintf("fixed %d\n", reviews.Load())
		}
		return os.WriteFile(filepath.Join(req.Dir, file), []byte(content), 0o600)
	}}, &fixes, &prompt
}

func redLint(id int64) []fakegh.CheckRun {
	return []fakegh.CheckRun{
		{Name: "test", Status: "completed", Conclusion: "success"},
		{ID: id, Name: "lint", Status: "completed", Conclusion: "failure"},
	}
}

func TestCIFixRoundThenMerge(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig)
	w.gh.SetChecks(redLint(42))
	w.gh.SetLog(42, "SETUP-LINE\n"+strings.Repeat("noise\n", 2000)+"lint: main.go:3: unused variable x\n")
	h, fixes, prompt := ciFixHarness(0, func() {
		w.gh.SetChecks([]fakegh.CheckRun{{Name: "lint", Status: "completed", Conclusion: "success"}})
	})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing merged]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if fixes.Load() != 1 {
		t.Errorf("CI fix sessions = %d, want 1", fixes.Load())
	}
	brief, _ := prompt.Load().(string)
	if !strings.Contains(brief, "unused variable x") || strings.Contains(brief, "SETUP-LINE") || !strings.Contains(brief, `"lint"`) {
		t.Errorf("CI fix brief does not carry the log's capped tail and the check name:\n%s", brief)
	}
	if _, tail, _ := strings.Cut(brief, "The end of its log:\n"); len(tail) > ciwatch.TailBytes+1000 {
		t.Errorf("CI fix brief carries %d bytes of log and instructions, want a tail capped at %d", len(tail), ciwatch.TailBytes)
	}
	if got := git(t, w.bare, "show", "7_fix-the-thing:ci.txt"); got != "fixed" {
		t.Errorf("pushed ci.txt = %q, want the fix on the pull request's branch", got)
	}
	if merges, _ := w.gh.Merges(); merges != 1 {
		t.Errorf("merges = %d, want 1", merges)
	}
	if pulls, _ := w.gh.Snapshot(); len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want only the one that was merged", pulls)
	}
}

func TestCIFixRoundRunsOnce(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig)
	w.gh.SetChecks(redLint(42))
	w.gh.SetLog(42, "lint failed\n")
	h, fixes, _ := ciFixHarness(0, func() {})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	deadline := time.Now().Add(5 * time.Second)
	for fixes.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the CI fix round did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if merges, _ := w.gh.Merges(); merges != 0 {
		t.Fatalf("merges = %d, want none while the check stays red", merges)
	}
	if fixes.Load() != 1 {
		t.Fatalf("CI fix sessions = %d, want exactly 1 before the check goes green", fixes.Load())
	}
	w.gh.SetChecks([]fakegh.CheckRun{{Name: "lint", Status: "completed", Conclusion: "success"}})

	states := w.await(t, "alice", 7)
	if got := states[len(states)-1]; got != StateMerged {
		t.Errorf("states = %v, want merged last", states)
	}
	assertSettled(t, w)
	if fixes.Load() != 1 {
		t.Errorf("CI fix sessions = %d, want exactly 1", fixes.Load())
	}
	if merges, _ := w.gh.Merges(); merges != 1 {
		t.Errorf("merges = %d, want 1", merges)
	}
	_, comments := w.gh.Snapshot()
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "schmeide merged the pull request") || strings.Contains(comments["7"][0], "left the pull request open") {
		t.Errorf("issue comments = %q, want the merged pull request and no give-up", comments["7"])
	}
}

// TestCIFixSkippedWhenRoundsSpent passes the review in its last round, so
// no round is left for a CI fix.
func TestCIFixSkippedWhenRoundsSpent(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig+"review_fix = \"on\"\n")
	w.gh.SetChecks(redLint(42))
	w.gh.SetLog(42, "lint failed\n")
	h, fixes, _ := ciFixHarness(defaultMaxReviewRounds-1, func() {})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("check runs were not read; calls %v", w.gh.Calls())
		}
		saw := false
		for _, call := range w.gh.Calls() {
			if strings.Contains(call, "check-runs") {
				saw = true
			}
		}
		if saw {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if fixes.Load() != 0 {
		t.Errorf("CI fix sessions = %d, want none once the rounds are spent", fixes.Load())
	}
	for _, call := range w.gh.Calls() {
		if strings.Contains(call, "/actions/jobs/") {
			t.Errorf("log read with no round left: %s", call)
		}
	}
	if merges, _ := w.gh.Merges(); merges != 0 {
		t.Fatalf("merges = %d, want none while the check stays red", merges)
	}
	w.gh.SetChecks([]fakegh.CheckRun{{Name: "lint", Status: "completed", Conclusion: "success"}})

	states := w.await(t, "alice", 7)
	if got := states[len(states)-1]; got != StateMerged {
		t.Errorf("states = %v, want merged last", states)
	}
	assertSettled(t, w)
	if fixes.Load() != 0 {
		t.Errorf("CI fix sessions = %d, want none once the rounds are spent", fixes.Load())
	}
}
