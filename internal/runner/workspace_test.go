package runner

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/secretscan"
)

func TestPushStopsForPrivateKeyWithoutRevealingIt(t *testing.T) {
	w, bare := newPushFixture(t)
	key := rsaPrivateKeyPEM(t)
	if err := os.WriteFile(filepath.Join(w.Dir(), "id_rsa"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(t.Context(), "add a key"); err != nil {
		t.Fatal(err)
	}
	err := w.Push(t.Context())
	if err == nil {
		t.Fatal("Push succeeded with a private key in a tracked file")
	}
	msg := err.Error()
	if !strings.Contains(msg, "id_rsa") || !strings.Contains(msg, secretscan.KindPEM) {
		t.Fatalf("Push error = %q, want the file and %s", msg, secretscan.KindPEM)
	}
	if strings.Contains(msg, "-----BEGIN") || strings.Contains(msg, "PRIVATE KEY") || strings.Contains(msg, pemBodySample(t, key)) {
		t.Fatal("Push error includes the private key")
	}
	if strings.Contains(msg, "git push") {
		t.Fatalf("Push error = %q, git push ran", msg)
	}
	if remoteHasBranch(t, bare, w.spec.branch) {
		t.Fatal("git push updated the remote")
	}
}

func TestPushAllowsALineMarkedAllowSecret(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat("a", 36)
	t.Setenv("GITHUB_TOKEN", token)
	w, bare := newPushFixture(t)
	line := "token = \"" + token + "\" // schmeide:allow-secret\n"
	if err := os.WriteFile(filepath.Join(w.Dir(), "allowed.txt"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(t.Context(), "allow the literal"); err != nil {
		t.Fatal(err)
	}
	if err := w.Push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !remoteHasBranch(t, bare, w.spec.branch) {
		t.Fatal("allowed line was not pushed")
	}
}

func TestPushPublishesACleanWorktree(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat("b", 36)
	t.Setenv("GITHUB_TOKEN", token)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKI"+"AIOSFODNN7EXAMPLE")
	w, bare := newPushFixture(t)
	if err := w.Push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !remoteHasBranch(t, bare, w.spec.branch) {
		t.Fatal("clean worktree was not pushed")
	}
}

func newPushFixture(t *testing.T) (*worktree, string) {
	t.Helper()
	// Keep the machine's git config (signing, hooks, identity) out of the test.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "config", "user.email", "dev@example.com")
	git(t, src, "config", "user.name", "Dev")
	if err := os.WriteFile(filepath.Join(src, "README"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "README")
	git(t, src, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	bare := filepath.Join(t.TempDir(), "origin.git")
	git(t, src, "clone", "-q", "--bare", src, bare)

	w, err := newWorktree(t.Context(), worktreeSpec{
		workDir:  t.TempDir(),
		url:      bare,
		base:     "main",
		branch:   "job",
		token:    "gh" + "p_" + strings.Repeat("e", 36),
		gitName:  "schmeide[bot]",
		gitEmail: "bot@example.com",
		id:       "job",
		repoID:   "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, bare
}

func rsaPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return pem.EncodeToMemory(block)
}

func pemBodySample(t *testing.T, block []byte) string {
	t.Helper()
	for line := range bytes.SplitSeq(block, []byte("\n")) {
		if len(line) >= 40 && !bytes.HasPrefix(line, []byte("-----")) {
			return string(line[:40])
		}
	}
	t.Fatal("private key PEM has no body")
	return ""
}

func remoteHasBranch(t *testing.T, bare, branch string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "--git-dir", bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func TestNewWorktreeGuardsOtherJobBranches(t *testing.T) {
	w, _ := newPushFixture(t)
	git(t, w.repoDir, "branch", "2_two", w.baseRef)
	two := git(t, w.repoDir, "rev-parse", "2_two")
	if err := os.WriteFile(filepath.Join(w.Dir(), "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := w.Commit(t.Context(), "add a"); err != nil || !changed {
		t.Fatalf("Commit = %v, %v; want a commit on the job branch", changed, err)
	}
	if out := git(t, w.Dir(), "ls-files"); strings.Contains(out, ".schmeide-refguard") {
		t.Fatalf("the hook was committed: %s", out)
	}
	if _, err := w.git(t.Context(), w.Dir(), "branch", "-f", "2_two", "HEAD"); err == nil {
		t.Fatal("the job worktree moved 2_two")
	}
	if got := git(t, w.repoDir, "rev-parse", "2_two"); got != two {
		t.Fatalf("2_two moved to %s", got)
	}
	if out, err := w.git(t.Context(), w.repoDir, "config", "core.hooksPath"); err == nil {
		t.Fatalf("the clone has core.hooksPath %s", out)
	}
	git(t, w.repoDir, "branch", "-f", "2_two", w.spec.branch)
}
