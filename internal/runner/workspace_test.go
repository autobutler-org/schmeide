package runner

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
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

func TestSecondWorktreeReusesCloneAndSharesGoCache(t *testing.T) {
	first, bare := newPushFixture(t)
	planted := filepath.Join(first.repoDir, "planted")
	if err := os.WriteFile(planted, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// git init copies its templates back in, so a missing description shows
	// a re-init even though init leaves the planted file alone.
	description := filepath.Join(first.repoDir, "description")
	if err := os.Remove(description); err != nil {
		t.Fatal(err)
	}
	second, err := newWorktree(t.Context(), worktreeSpec{
		workDir: filepath.Dir(filepath.Dir(first.repoDir)),
		url:     bare,
		base:    "main",
		branch:  "job-2",
		id:      "job-2",
		repoID:  first.spec.repoID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := os.Stat(planted); err != nil {
		t.Fatalf("the bare clone was re-created: %v", err)
	}
	if _, err := os.Stat(description); err == nil {
		t.Fatal("git init ran again on the existing bare clone")
	}

	out, err := second.Run(t.Context(), `echo "GOCACHE=$GOCACHE"; echo "GOMODCACHE=$GOMODCACHE"`)
	if err != nil {
		t.Fatal(err)
	}
	gate := strings.Fields(out)
	req := (&Runner{}).session(second.Dir(), "prompt")
	cacheDir := filepath.Join(filepath.Dir(filepath.Dir(first.repoDir)), "cache", "1")
	for i, name := range []string{"GOCACHE", "GOMODCACHE"} {
		session := lastEnv(req.Env, name)
		if gate[i] != session || !strings.HasPrefix(session, name+"="+cacheDir) {
			t.Errorf("gate %s, session %s, want both under %s", gate[i], session, cacheDir)
		}
	}
}

func lastEnv(env []string, name string) string {
	got := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, name+"=") {
			got = entry
		}
	}
	return got
}

func newPushFixture(t *testing.T) (*worktree, string) {
	t.Helper()
	return newPushFixtureOn(t, nil)
}

// newPushFixtureOn is newPushFixture with base, file name to content, also
// committed on main before the job's worktree is cut.
func newPushFixtureOn(t *testing.T, base map[string]string) (*worktree, string) {
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
	for name, body := range base {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, src, "add", "-A")
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

func TestPushIgnoresALiteralAlreadyOnTheBase(t *testing.T) {
	old := "AKI" + "AIOSFODNN7EXAMPLE"
	w, bare := newPushFixtureOn(t, map[string]string{"legacy.txt": "key " + old + "\n"})
	if err := os.WriteFile(filepath.Join(w.Dir(), "legacy.txt"), []byte("key "+old+"\nnew line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(t.Context(), "touch the file"); err != nil {
		t.Fatal(err)
	}
	if err := w.Push(t.Context()); err != nil {
		t.Fatalf("Push = %v, want the base's literal ignored", err)
	}
	if !remoteHasBranch(t, bare, w.spec.branch) {
		t.Fatal("the branch was not pushed")
	}
}

func TestPushRefusesTheJobsCredentialsAndNewTokenShapes(t *testing.T) {
	const nats = "steward-nats-token-no-shape"
	tests := []struct {
		name string
		line func(w *worktree) string
		kind string
	}{
		{name: "installation token verbatim", line: func(w *worktree) string { return "t=" + w.spec.token + " // schmeide:allow-secret" }, kind: secretscan.KindCredential},
		{name: "NATS token verbatim", line: func(*worktree) string { return "nats " + nats }, kind: secretscan.KindCredential},
		{name: "another installation token", line: func(*worktree) string { return "gh" + "s_" + strings.Repeat("Q", 40) }, kind: secretscan.KindGitHub},
		{name: "Anthropic key", line: func(*worktree) string { return "sk-" + "ant-" + strings.Repeat("a", 30) }, kind: secretscan.KindAnthropic},
		{name: "xAI key", line: func(*worktree) string { return "xa" + "i-" + strings.Repeat("b", 30) }, kind: secretscan.KindXAI},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, bare := newPushFixture(t)
			w.spec.secrets = []string{nats}
			line := tt.line(w)
			if err := os.WriteFile(filepath.Join(w.Dir(), "leak.txt"), []byte(line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Commit(t.Context(), "leak"); err != nil {
				t.Fatal(err)
			}
			err := w.Push(t.Context())
			if err == nil {
				t.Fatal("Push succeeded with a credential in an added line")
			}
			want := "refusing to push: leak.txt:1 contains a " + tt.kind
			if err.Error() != want {
				t.Fatalf("Push error = %q, want %q", err, want)
			}
			if strings.Contains(err.Error(), w.spec.token) || strings.Contains(err.Error(), nats) {
				t.Fatal("Push error includes the secret text")
			}
			if remoteHasBranch(t, bare, w.spec.branch) {
				t.Fatal("git push updated the remote")
			}
		})
	}
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

func TestGateRunsInAScratchHomeThatCloseRemoves(t *testing.T) {
	t.Setenv("SCHMEIDE_NATS_TOKEN", "nats-secret")
	w, _ := newPushFixture(t)
	w.spec.gateEnv = gateEnv(os.Environ(), nil)
	out, err := w.Run(t.Context(), `touch "$HOME/dotfile" "$TMPDIR/tmpfile" && echo "$HOME|$TMPDIR|$SCHMEIDE_NATS_TOKEN"`)
	if err != nil {
		t.Fatalf("gate: %v: %s", err, out)
	}
	if got, want := strings.TrimSpace(out), w.scratch+"|"+w.scratch+"|"; got != want {
		t.Errorf("gate saw HOME|TMPDIR|SCHMEIDE_NATS_TOKEN = %q, want %q", got, want)
	}
	if strings.HasPrefix(w.scratch, w.dir) {
		t.Errorf("scratch %s is inside the checkout %s, so a commit would take it", w.scratch, w.dir)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.scratch); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch %s survived Close: %v", w.scratch, err)
	}
}
