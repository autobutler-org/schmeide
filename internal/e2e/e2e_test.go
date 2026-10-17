//go:build e2e

// Package e2e drives the built schmeide binary through whole journeys:
// `schmeide control` and `schmeide runner` run as subprocesses, GitHub is
// internal/testutil/fakegh, the origin is a local bare repository, and the
// claude and grok CLIs are cmd/fakeagent. Run it with `make test/e2e`.
package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/testutil/fakegh"
)

const (
	repo           = "acme/widget"
	repoID         = 42
	installationID = 7
	steward        = "octo"
	runnerToken    = "runner-token"
	jobToken       = "ghs_e2e"
	webhookSecret  = "e2e-secret"
)

// binDir holds schmeide, fakeagent, and the claude and grok wrappers.
var binDir string

// wrappers stand in for the agent CLIs. A login check answers as logged in,
// because fakeagent writes nothing to stdout. Anything else is a session and
// runs fakeagent with the script for that CLI. The variable prefixes are ones
// the runner already passes to a harness session.
var wrappers = map[string]string{
	"claude": `#!/bin/sh
if [ "$1" = auth ]; then echo '{"loggedIn":true}'; exit 0; fi
FAKEAGENT_SCRIPT=$CLAUDE_E2E_SCRIPT exec "$(dirname "$0")/fakeagent"
`,
	"grok": `#!/bin/sh
if [ "$1" = models ]; then echo 'You are authenticated via schmeide e2e.'; exit 0; fi
FAKEAGENT_SCRIPT=$XAI_E2E_SCRIPT exec "$(dirname "$0")/fakeagent"
`,
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "schmeide-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	for name, pkg := range map[string]string{"schmeide": "./cmd/schmeide", "fakeagent": "./cmd/fakeagent"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, name), pkg)
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", name, err, out)
			return 1
		}
	}
	for name, script := range wrappers {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	binDir = dir
	return m.Run()
}

// logBuf collects a subprocess's output while the test reads it.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// proc is a running schmeide subprocess.
type proc struct {
	name string
	cmd  *exec.Cmd
	log  *logBuf
	done chan struct{}
}

// world is one journey's temp dir, origin, fake GitHub, and control node.
type world struct {
	t        *testing.T
	dir      string
	gh       *fakegh.Server
	natsAddr string
	httpAddr string
	control  *proc
	runners  []*proc
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// git runs git without the user's or the system's config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=e2e", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=e2e", "GIT_COMMITTER_EMAIL=e2e@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newWorld seeds an origin whose repo config runs gate, starts the fakes, and
// starts the control node.
func newWorld(t *testing.T, gate string) *world {
	w := &world{t: t, dir: t.TempDir(), natsAddr: freeAddr(t), httpAddr: freeAddr(t)}
	t.Cleanup(w.stop)

	origin := filepath.Join(w.dir, "origin", repo+".git")
	seed := filepath.Join(w.dir, "seed")
	for _, d := range []string{origin, filepath.Join(seed, ".schmeide"), filepath.Join(w.dir, "home")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	writeFile(t, filepath.Join(seed, "README.md"), "# widget\n", 0o644)
	writeFile(t, filepath.Join(seed, ".schmeide", "config.toml"), fmt.Sprintf("gate = %q\n", gate), 0o644)
	git(t, seed, "init", "-q", "-b", "main")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", origin, "main")

	w.gh = fakegh.New(t, repo, jobToken)
	minter := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != fmt.Sprintf("/app/installations/%d/access_tokens", installationID) {
			t.Errorf("unexpected token call %s %s", r.Method, r.URL)
			http.NotFound(rw, r)
			return
		}
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(map[string]string{"token": jobToken, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	}))
	t.Cleanup(minter.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(w.dir, "app.pem")
	writeFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), 0o600)

	w.control = w.start("control", nil, "control",
		"--http-addr", w.httpAddr,
		"--nats-addr", w.natsAddr,
		"--data-dir", filepath.Join(w.dir, "data"),
		"--webhook-secret", webhookSecret,
		"--runner-tokens", steward+":"+runnerToken,
		"--allowed-orgs", "acme",
		"--github-app-id", "1",
		"--github-app-private-key-file", keyFile,
		"--github-api-url", minter.URL)
	w.waitLog(w.control, "control node started", 1)
	return w
}

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// start runs the schmeide binary with a minimal environment plus env.
func (w *world) start(name string, env []string, args ...string) *proc {
	w.t.Helper()
	p := &proc{name: name, log: &logBuf{}, done: make(chan struct{})}
	p.cmd = exec.Command(filepath.Join(binDir, "schmeide"), args...)
	p.cmd.Dir = w.dir
	p.cmd.Env = append([]string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + filepath.Join(w.dir, "home"),
		"TMPDIR=" + w.dir,
	}, env...)
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	// A session left behind by a killed runner may hold the output open.
	p.cmd.WaitDelay = 5 * time.Second
	if err := p.cmd.Start(); err != nil {
		w.t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	return p
}

// startRunner starts a runner whose claude sessions run writer and whose grok
// sessions, the reviewer, run reviewer. It returns once control has seen the
// registration, so the job goes on the steward's subject and not the pool's.
func (w *world) startRunner(writer, reviewer string) *proc {
	w.t.Helper()
	p := w.start(fmt.Sprintf("runner %d", len(w.runners)+1), []string{
		"SCHMEIDE_TRUST=1",
		"CLAUDE_E2E_SCRIPT=" + writer,
		"XAI_E2E_SCRIPT=" + reviewer,
	}, "runner",
		"--steward", steward,
		"--nats-url", "nats://"+w.natsAddr,
		"--nats-token", runnerToken,
		"--work-dir", filepath.Join(w.dir, "work"),
		"--github-api-url", w.gh.URL,
		"--github-url", "file://"+filepath.Join(w.dir, "origin"),
		// A killed runner's job is redelivered after this, not the 5m default.
		"--ack-wait", "3s")
	w.runners = append(w.runners, p)
	w.waitLog(w.control, "runner registered", len(w.runners))
	return p
}

// stop ends every subprocess and prints their logs when the journey failed.
func (w *world) stop() {
	for _, p := range append(slices.Clone(w.runners), w.control) {
		if p == nil {
			continue
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(15 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if w.t.Failed() {
			w.t.Logf("--- %s log ---\n%s", p.name, p.log.String())
		}
	}
}

// waitFor polls cond until it holds or timeout passes.
func (w *world) waitFor(what string, timeout time.Duration, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitLog waits until p's log has text at least n times.
func (w *world) waitLog(p *proc, text string, n int) {
	w.t.Helper()
	w.waitFor(fmt.Sprintf("%s to log %q %d times", p.name, text, n), 30*time.Second, func() bool {
		select {
		case <-p.done:
			w.t.Fatalf("%s exited early: %v\n%s", p.name, p.cmd.ProcessState, p.log.String())
		default:
		}
		return strings.Count(p.log.String(), text) >= n
	})
}

// label delivers the webhook GitHub sends when issue gets the ready label
// while it is assigned to the steward.
func (w *world) label(issue int, title string) {
	w.t.Helper()
	body, err := json.Marshal(map[string]any{
		"action": "labeled",
		"issue": map[string]any{
			"number": issue, "title": title, "body": "Make it so.", "state": "open",
			"assignee": map[string]string{"login": steward},
			"labels":   []map[string]string{{"name": "schmeide:ready"}},
		},
		"repository":   map[string]any{"id": repoID, "full_name": repo},
		"installation": map[string]any{"id": installationID},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodPost, "http://"+w.httpAddr+"/webhook", bytes.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("X-Github-Event", "issues")
	req.Header.Set("X-Github-Delivery", "delivery-"+strconv.Itoa(issue))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		w.t.Fatalf("webhook status = %s, want 202 Accepted (job created)", resp.Status)
	}
}

// waitComment waits for a comment on issue that contains text.
func (w *world) waitComment(issue int, text string, timeout time.Duration) {
	w.t.Helper()
	w.waitFor(fmt.Sprintf("a comment on #%d containing %q", issue, text), timeout, func() bool {
		_, comments := w.gh.Snapshot()
		return slices.ContainsFunc(comments[strconv.Itoa(issue)], func(c string) bool { return strings.Contains(c, text) })
	})
}

func (w *world) pulls() []fakegh.Pull {
	pulls, _ := w.gh.Snapshot()
	return pulls
}

// writer is the worker session that succeeds: one change and the pull
// request text the runner reads from .schmeide/pr.md.
const writer = "edit greeting.txt hello\nedit .schmeide/pr.md feat: add a greeting"

const passReview = "edit .schmeide/review.md pass"

func TestJourneyLabeledIssueOpensOnePullRequest(t *testing.T) {
	w := newWorld(t, "true")
	r := w.startRunner(writer, passReview)
	w.label(1, "Add a greeting")

	w.waitComment(1, "The review passed.", time.Minute)
	if !strings.Contains(r.log.String(), "state=claimed") {
		t.Errorf("the runner did not report claiming the job")
	}
	pulls := w.pulls()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want one", pulls)
	}
	if p := pulls[0]; p.Head != "1_add-a-greeting" || p.Base != "main" || p.Title != "feat: add a greeting" || !strings.Contains(p.Body, "Closes #1") {
		t.Errorf("pull request = %+v", p)
	}
	if refs := git(t, w.dir, "ls-remote", filepath.Join(w.dir, "origin", repo+".git"), "refs/heads/1_add-a-greeting"); refs == "" {
		t.Error("the branch was not pushed to the origin")
	}
}

func TestJourneyGateFailureCommentsAndOpensNoPullRequest(t *testing.T) {
	w := newWorld(t, "false")
	w.startRunner(writer, passReview)
	w.label(2, "Add a greeting")

	w.waitComment(2, "did not open a pull request: the gate command `false` failed", time.Minute)
	if pulls := w.pulls(); len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
}

func TestJourneyKilledRunnerJobIsRedeliveredWithoutASecondPullRequest(t *testing.T) {
	w := newWorld(t, "true")
	pidFile := filepath.Join(w.dir, "reviewer.pid")
	// The first reviewer records its PID and hangs, so the runner is killed
	// after the pull request is open and before the job is acknowledged.
	first := w.startRunner(writer, "edit "+pidFile+" $$\nhang")
	w.label(3, "Add a greeting")

	w.waitFor("the first reviewer to start", time.Minute, func() bool {
		_, err := os.Stat(pidFile)
		return err == nil && len(w.pulls()) == 1
	})
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// The reviewer has its own process group, so killing the runner leaves it.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	<-first.done

	// The job is redelivered once its ack wait (3s) runs out with no heartbeat.
	second := w.startRunner(writer, passReview)
	w.waitComment(3, "The review passed.", 2*time.Minute)
	if !strings.Contains(second.log.String(), "state=claimed") {
		t.Errorf("the job was not redelivered to the second runner")
	}
	if pulls := w.pulls(); len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want the first runner's one only", pulls)
	}
}
