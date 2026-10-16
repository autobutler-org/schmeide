package refguard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/refguard"
)

func TestJobWorktreeMovesOnlyItsOwnBranch(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	clone := t.TempDir()
	git(t, clone, "init", "-q", "-b", "main")
	git(t, clone, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, clone, "branch", "1_one")
	git(t, clone, "branch", "2_two")
	git(t, clone, "branch", "10x_not-a-job")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, clone, "worktree", "add", "-q", wt, "1_one")
	hooks, err := refguard.Install(wt, "1_one")
	if err != nil {
		t.Fatal(err)
	}
	git(t, clone, "config", "extensions.worktreeConfig", "true")
	git(t, wt, "config", "--worktree", "core.hooksPath", hooks)

	git(t, wt, "commit", "-q", "--allow-empty", "-m", "on one")
	if out := git(t, wt, "status", "--porcelain", "--ignored=no"); out != "" {
		t.Fatalf("the hooks directory shows in git status: %q", out)
	}
	two := git(t, clone, "rev-parse", "2_two")
	for _, args := range [][]string{
		{"branch", "-f", "2_two", "1_one"},
		{"update-ref", "refs/heads/2_two", "1_one"},
		{"branch", "-D", "2_two"},
		{"branch", "3_three"},
	} {
		out, err := run(wt, args...)
		if err == nil {
			t.Fatalf("git %v succeeded in the job worktree", args)
		}
		if !strings.Contains(out, "may move only refs/heads/1_one") {
			t.Fatalf("git %v output = %q, want the guard's message", args, out)
		}
	}
	if got := git(t, clone, "rev-parse", "2_two"); got != two {
		t.Fatalf("2_two moved to %s", got)
	}
	git(t, wt, "branch", "-f", "10x_not-a-job", "1_one")

	git(t, clone, "branch", "-f", "2_two", "1_one")
	if got := git(t, clone, "rev-parse", "2_two"); got == two {
		t.Fatal("the main clone could not move 2_two")
	}
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "user.name=Dev", "-c", "user.email=dev@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}
