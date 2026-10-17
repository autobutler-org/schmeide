package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
)

// stubWorkspace is a Workspace over a plain directory whose gate passes.
type stubWorkspace struct{ dir string }

func (s stubWorkspace) Dir() string                                { return s.dir }
func (stubWorkspace) Commit(context.Context, string) (bool, error) { return true, nil }
func (stubWorkspace) Run(context.Context, string) (string, error)  { return "", nil }
func (stubWorkspace) Push(context.Context) error                   { return nil }
func (stubWorkspace) Head(context.Context) (string, error)         { return "", nil }
func (stubWorkspace) Resume(context.Context) error                 { return nil }
func (stubWorkspace) Count(context.Context, string) (int, error)   { return 0, nil }
func (stubWorkspace) Close() error                                 { return nil }

// verdictFake writes a "changes" verdict on a review brief and does nothing
// on any other brief.
func verdictFake(checkErr error) *harness.Fake {
	return &harness.Fake{CheckErr: checkErr, RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "changes\nfix it\n")
		}
		return nil
	}}
}

// reviewRound runs one review and one fix with writer as r.harness.
func reviewRound(t *testing.T, writer harness.Harness, all ...harness.Harness) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".schmeide"), 0o750); err != nil {
		t.Fatal(err)
	}
	r := &Runner{harness: writer, opts: Options{Harnesses: all}, log: slog.New(slog.DiscardHandler)}
	ws := stubWorkspace{dir: dir}
	job := control.Job{Repo: testRepo, Issue: 1, Title: "t"}
	verdict, findings, err := r.reviewOnce(t.Context(), ws, job, 0)
	if err != nil || verdict != verdictChanges {
		t.Fatalf("review = %v, %q, %v; want changes", verdict, findings, err)
	}
	if out, stop := r.applyFindings(t.Context(), ws, job, "true", "pr", findings, deadlines{}); stop {
		t.Fatalf("fix stopped: %+v", out)
	}
}

func prompts(h *harness.Fake) []string {
	var ps []string
	for _, req := range h.Requests {
		ps = append(ps, req.Prompt)
	}
	return ps
}

func TestReviewRunsOnTheOtherReadyHarness(t *testing.T) {
	writer, other := verdictFake(nil), verdictFake(nil)
	reviewRound(t, writer, writer, other)

	if ps := prompts(other); len(ps) != 1 || !strings.Contains(ps[0], reviewLead) {
		t.Errorf("other harness ran %q, want only the review session", ps)
	}
	if ps := prompts(writer); len(ps) != 1 || strings.Contains(ps[0], reviewLead) {
		t.Errorf("writer harness ran %q, want only the fix session", ps)
	}
}

func TestReviewStaysOnWriterWhenNoOtherHarnessIsReady(t *testing.T) {
	writer, other := verdictFake(nil), verdictFake(errors.New("not logged in"))
	reviewRound(t, writer, writer, other)

	if ps := prompts(other); len(ps) != 0 {
		t.Errorf("unready harness ran %q, want nothing", ps)
	}
	if ps := prompts(writer); len(ps) != 2 || !strings.Contains(ps[0], reviewLead) || strings.Contains(ps[1], reviewLead) {
		t.Errorf("writer harness ran %q, want the review then the fix session", ps)
	}
}

// TestSessionPromptIncludesTrackedFile checks the prompt session builds for
// every harness session, including the reviewer.
func TestSessionPromptIncludesTrackedFile(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "dev@example.com")
	runGit(t, dir, "config", "user.name", "Dev")
	body := "package p\n\nfunc Hello() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "mapped.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "mapped.go")
	runGit(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "add mapped.go")

	const brief = "Fix the widget."
	req := (&Runner{}).session(dir, brief)
	if !strings.Contains(req.Prompt, "mapped.go") {
		t.Fatalf("prompt = %q, want the tracked Go file", req.Prompt)
	}
	if !strings.Contains(req.Prompt, brief) {
		t.Fatalf("prompt = %q, want the brief", req.Prompt)
	}
}

func TestSessionPromptUnchangedWithoutAMap(t *testing.T) {
	const brief = "Fix the widget."
	req := (&Runner{}).session(t.TempDir(), brief)
	if req.Prompt != brief {
		t.Fatalf("prompt = %q, want the brief alone", req.Prompt)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
