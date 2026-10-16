package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/autobutler-org/schmeide/internal/secretscan"
)

// Workspace is one job's isolated checkout, already on the job's branch. It
// is everything the job flow needs from wherever the job runs: git worktrees
// on the host today, and a container backend later.
type Workspace interface {
	// Dir is the checkout the harness session works in.
	Dir() string
	// Commit commits whatever is uncommitted and reports whether the branch
	// now differs from its base.
	Commit(ctx context.Context, message string) (changed bool, err error)
	// Run runs a shell command in the checkout and returns its combined
	// output. It is used for the gate.
	Run(ctx context.Context, command string) (output string, err error)
	// Push publishes the branch.
	Push(ctx context.Context) error
	// Head is the commit the branch points at.
	Head(ctx context.Context) (string, error)
	// Resume moves the branch to the remote branch's head, for a job whose
	// pull request is already open.
	Resume(ctx context.Context) error
	// Count is how many commits after the base have subject as their subject.
	Count(ctx context.Context, subject string) (int, error)
	// Close removes the workspace.
	Close() error
}

type worktreeSpec struct {
	workDir, url, base, branch, token string
	gitName, gitEmail                 string
	// id names the worktree directory and repoID the clone it hangs off.
	id, repoID string
	// env is the environment for git and the gate, without the runner's own
	// settings and secrets.
	env []string
}

// worktree is the [Workspace] backed by a git worktree on the host.
type worktree struct {
	spec          worktreeSpec
	repoDir, dir  string
	env           []string
	baseRef, head string
}

var _ Workspace = (*worktree)(nil)

// newWorktree fetches the base branch into the runner's clone of the
// repository and checks it out on the job's branch. Anything an earlier,
// interrupted attempt left behind is replaced.
func newWorktree(ctx context.Context, s worktreeSpec) (*worktree, error) {
	w := &worktree{
		spec:    s,
		repoDir: filepath.Join(s.workDir, "repos", s.repoID+".git"),
		dir:     filepath.Join(s.workDir, "worktrees", s.id),
		baseRef: "refs/remotes/origin/" + s.base,
		// The token goes in through the environment so that it is never
		// written to git config or shown in the process list.
		env: append(slices.Clone(s.env),
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+s.token)),
			"GIT_AUTHOR_NAME="+s.gitName, "GIT_AUTHOR_EMAIL="+s.gitEmail,
			"GIT_COMMITTER_NAME="+s.gitName, "GIT_COMMITTER_EMAIL="+s.gitEmail,
		),
	}
	if err := os.MkdirAll(w.repoDir, 0o750); err != nil {
		return nil, fmt.Errorf("create the work directory: %w; check that --work-dir is writable", err)
	}
	if err := os.RemoveAll(w.dir); err != nil {
		return nil, fmt.Errorf("clear a leftover worktree: %w", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "--bare"},
		{"worktree", "prune"},
		{"fetch", "-q", s.url, "+refs/heads/" + s.base + ":" + w.baseRef},
		{"worktree", "add", "-q", "-B", s.branch, w.dir, w.baseRef},
	} {
		if _, err := w.git(ctx, w.repoDir, args...); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func (w *worktree) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Dir implements [Workspace].
func (w *worktree) Dir() string { return w.dir }

// Commit implements [Workspace].
func (w *worktree) Commit(ctx context.Context, message string) (bool, error) {
	if _, err := w.git(ctx, w.dir, "add", "-A"); err != nil {
		return false, err
	}
	dirty, err := w.git(ctx, w.dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if dirty != "" {
		// The commit belongs to the GitHub App, so it is not signed with
		// whatever key the machine's owner has configured.
		if _, err := w.git(ctx, w.dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", message); err != nil {
			return false, err
		}
	}
	ahead, err := w.git(ctx, w.dir, "rev-list", "--count", w.baseRef+"..HEAD")
	return ahead != "0", err
}

// Run implements [Workspace].
func (w *worktree) Run(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = w.dir
	cmd.Env = w.spec.env
	// Its own process group, so a canceled gate takes its children with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Push implements [Workspace]. A missing remote branch, or one whose head is
// an ancestor of HEAD, is updated without force. That is how a review round
// publishes another commit on the same branch. A remote head that is not an
// ancestor was not written by this checkout, so it is left in place and the
// push fails. Tracked files are scanned before git push. A credential-shaped
// literal refuses the push, and git push does not run.
func (w *worktree) Push(ctx context.Context) error {
	found, err := secretscan.Scan(ctx, w.dir)
	if err != nil {
		return fmt.Errorf("scan tracked files for secrets: %w", err)
	}
	if len(found) > 0 {
		return secretLiteralsError(found)
	}
	ref := "refs/heads/" + w.spec.branch
	remote, err := w.remoteHead(ctx, ref)
	if err != nil {
		return err
	}
	if remote != "" && !w.ancestorOfHEAD(ctx, remote) {
		return fmt.Errorf("the branch %s moved on the remote and was not overwritten; someone has to look at that branch", w.spec.branch)
	}
	_, err = w.git(ctx, w.dir, "push", "-q", w.spec.url, "HEAD:"+ref)
	return err
}

// secretLiteralsError names each file and the kind of secret. It does not
// include the secret text.
func secretLiteralsError(found []secretscan.Finding) error {
	parts := make([]string, len(found))
	for i, finding := range found {
		parts[i] = fmt.Sprintf("%s:%d contains a %s", finding.File, finding.Line, finding.Kind)
	}
	return fmt.Errorf("refusing to push: %s", strings.Join(parts, "; "))
}

// remoteHead is the commit the remote branch points at, or "" when that
// branch does not exist.
func (w *worktree) remoteHead(ctx context.Context, ref string) (string, error) {
	out, err := w.git(ctx, w.dir, "ls-remote", w.spec.url, ref)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		sha, got, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && got == ref && sha != "" {
			return sha, nil
		}
	}
	return "", nil
}

// ancestorOfHEAD reports whether commit is an ancestor of HEAD, including
// HEAD itself. A commit this checkout does not contain cannot be one.
func (w *worktree) ancestorOfHEAD(ctx context.Context, commit string) bool {
	if _, err := w.git(ctx, w.dir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return false
	}
	_, err := w.git(ctx, w.dir, "merge-base", "--is-ancestor", commit, "HEAD")
	return err == nil
}

// Head implements [Workspace].
func (w *worktree) Head(ctx context.Context) (string, error) {
	return w.git(ctx, w.dir, "rev-parse", "HEAD")
}

// Resume implements [Workspace].
func (w *worktree) Resume(ctx context.Context) error {
	ref := "refs/remotes/origin/" + w.spec.branch
	if _, err := w.git(ctx, w.repoDir, "fetch", "-q", w.spec.url, "+refs/heads/"+w.spec.branch+":"+ref); err != nil {
		return err
	}
	_, err := w.git(ctx, w.dir, "reset", "-q", "--hard", ref)
	return err
}

// Count implements [Workspace].
func (w *worktree) Count(ctx context.Context, subject string) (int, error) {
	out, err := w.git(ctx, w.dir, "log", "--format=%s", w.baseRef+"..HEAD")
	if err != nil {
		return 0, err
	}
	n := 0
	for line := range strings.SplitSeq(out, "\n") {
		if line == subject {
			n++
		}
	}
	return n, nil
}

// Close implements [Workspace].
func (w *worktree) Close() error {
	// Cleanup still has to happen when the job's context was canceled.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, rmErr := w.git(ctx, w.repoDir, "worktree", "remove", "--force", w.dir)
	_, brErr := w.git(ctx, w.repoDir, "branch", "-q", "-D", w.spec.branch)
	return errors.Join(rmErr, brErr)
}
