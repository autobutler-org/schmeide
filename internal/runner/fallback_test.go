package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
)

// outOfUsage is a fake harness named name whose every session writes
// partial.txt and then stops at a usage limit.
func outOfUsage(name string) *harness.Fake {
	return &harness.Fake{HarnessName: name, RunFunc: func(_ context.Context, req harness.Request) error {
		if err := os.WriteFile(filepath.Join(req.Dir, "partial.txt"), []byte("half\n"), 0o600); err != nil {
			return err
		}
		return &harness.UsageLimitError{Harness: name, Tail: name + " got this far"}
	}}
}

func TestUsageLimitContinuesTheJobOnTheNextHarness(t *testing.T) {
	w := newWorld(t)
	first := outOfUsage("first")
	second, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	second.HarnessName = "second"
	w.more = []harness.Harness{second}
	w.start(t, "alice", first)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	if len(first.Requests) != 1 {
		t.Errorf("first ran %d sessions, want 1: an exhausted harness is not used again in the job", len(first.Requests))
	}
	if len(second.Requests) != 2 {
		t.Fatalf("second ran %d sessions, want the continued worker session and the review", len(second.Requests))
	}
	prompt := second.Requests[0].Prompt
	for _, want := range []string{continueLead, "The first harness", "?? partial.txt", "first got this far", "Fix the Thing!"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("continued prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(second.Requests[1].Prompt, continueLead) {
		t.Error("the review session got a continuation note; only the continued session should")
	}
	const branch = "7_fix-the-thing"
	if files := git(t, w.bare, "ls-tree", "-r", "--name-only", branch); !strings.Contains(files, "partial.txt") || !strings.Contains(files, "fix.txt") {
		t.Errorf("pushed files = %s, want both the partial work and the continued work", files)
	}
}

func TestEveryHarnessOutOfUsageFailsTheJobAndLaterJobs(t *testing.T) {
	w := newWorld(t)
	first, second := outOfUsage("first"), outOfUsage("second")
	w.more = []harness.Harness{second}
	w.start(t, "alice", first)
	const want = "every ready harness is at its usage limit: first, second; this runner refuses them until it restarts"

	w.publish(t, "alice", 7)
	if got := fmt.Sprint(w.await(t, "alice", 7)); got != "[claimed working failed]" {
		t.Errorf("states = %s, want a final failure with no retry", got)
	}
	// A later job runs no session: both harnesses stay refused.
	w.publish(t, "alice", 8)
	if got := fmt.Sprint(w.await(t, "alice", 8)); got != "[claimed working failed]" {
		t.Errorf("later job states = %s, want a final failure", got)
	}
	if len(first.Requests) != 1 || len(second.Requests) != 1 {
		t.Errorf("sessions = %d first, %d second; want 1 each", len(first.Requests), len(second.Requests))
	}
	_, comments := w.gh.Snapshot()
	for _, issue := range []string{"7", "8"} {
		if len(comments[issue]) != 1 || !strings.Contains(comments[issue][0], want) {
			t.Errorf("issue %s comments = %q, want one with %q", issue, comments[issue], want)
		}
	}
}

func TestExhaustedHarnessStaysRefusedForLaterJobs(t *testing.T) {
	w := newWorld(t)
	first := outOfUsage("first")
	second, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	second.HarnessName = "second"
	w.more = []harness.Harness{second}
	w.start(t, "alice", first)
	for _, issue := range []int{7, 8} {
		w.publish(t, "alice", issue)
		if got := fmt.Sprint(w.await(t, "alice", issue)); got != "[claimed working reviewing pr_opened]" {
			t.Errorf("issue %d states = %s", issue, got)
		}
	}
	if len(first.Requests) != 1 {
		t.Errorf("first ran %d sessions, want 1: it stays refused after its limit", len(first.Requests))
	}
	if strings.Contains(second.Requests[len(second.Requests)-2].Prompt, continueLead) {
		t.Error("the later job's session got a continuation note; it should start fresh on second")
	}
}

func TestReviewSessionContinuesOnTheNextHarness(t *testing.T) {
	tests := []struct {
		name string
		// others are offered after the writer and the harness that runs out.
		others     []harness.Harness
		wantWriter int
	}{
		{"another harness takes the review", []harness.Harness{verdictFake(nil)}, 0},
		{"the writer takes the review when nothing else is left", nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, ".schmeide"), 0o750); err != nil {
				t.Fatal(err)
			}
			writer := verdictFake(nil)
			limited := outOfUsage("limited")
			r := &Runner{harness: writer, opts: Options{Harnesses: append([]harness.Harness{writer, limited}, tt.others...)}, log: slog.New(slog.DiscardHandler)}
			verdict, _, err := r.reviewOnce(t.Context(), stubWorkspace{dir: dir}, control.Job{Repo: testRepo, Issue: 1, Title: "t"}, 0)
			if err != nil || verdict != verdictChanges {
				t.Fatalf("review = %v, %v; want changes", verdict, err)
			}
			if len(writer.Requests) != tt.wantWriter {
				t.Errorf("writer ran %d sessions, want %d", len(writer.Requests), tt.wantWriter)
			}
			took := writer
			if len(tt.others) > 0 {
				took = tt.others[0].(*harness.Fake)
			}
			if len(took.Requests) != 1 || !strings.Contains(took.Requests[0].Prompt, continueLead) || !strings.Contains(took.Requests[0].Prompt, reviewLead) {
				t.Errorf("the harness that took over got %+v, want one continued review", took.Requests)
			}
			if r.harness != writer {
				t.Error("a review fallback changed the job's writer")
			}
		})
	}
}
