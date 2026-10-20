package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/ciwatch"
	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/repoconfig"
	"github.com/autobutler-org/schmeide/internal/testutil/fakegh"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var testRunnerTokens = map[string]string{"alice": "alice-nats", "bob": "bob-nats"}

const (
	testToken  = "job-token"
	testRepo   = "o/r"
	testRepoID = 42
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type pull = fakegh.Pull

type statusMsg struct {
	subject string
	Status
}

// world is a control node queue, a fake GitHub, and a git remote holding
// testRepo, whose gate fails when a file named "broken" exists.
type world struct {
	queue    *control.Queue
	natsURL  string
	gh       *fakegh.Server
	apiURL   string
	gitRoot  string
	bare     string
	client   *nats.Conn
	statuses chan statusMsg
	tracker  *control.Tracker
	// mint, when set, answers a job's token request. Nil mints testToken.
	mint func(context.Context, control.Job) (control.Token, error)
	// maxAttempts is the runner's --max-attempts. Zero keeps the default.
	maxAttempts int
	// ackWait and stuckAfter override the runner's delivery lease settings.
	ackWait, stuckAfter time.Duration
}

func newWorld(t *testing.T) *world {
	t.Helper()
	// Keep the machine's git config (signing, hooks, identity) out of the test.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	// Pick a free port for the embedded NATS server.
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	queue, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: testRunnerTokens})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	log := slog.New(slog.DiscardHandler)
	w := &world{
		queue:    queue,
		natsURL:  "nats://" + addr,
		gh:       fakegh.New(t, testRepo, testToken),
		gitRoot:  t.TempDir(),
		statuses: make(chan statusMsg, 64),
	}
	if err := queue.ServeTokens(func(ctx context.Context, job control.Job) (control.Token, error) {
		if w.mint != nil {
			return w.mint(ctx, job)
		}
		return control.Token{Token: testToken}, nil
	}, log); err != nil {
		t.Fatal(err)
	}
	w.apiURL = w.gh.URL

	w.bare = filepath.Join(w.gitRoot, "o", "r.git")
	git(t, "", "init", "-q", "--bare", "-b", "main", w.bare)
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main")
	if err := os.Mkdir(filepath.Join(src, ".schmeide"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".schmeide", "config.toml"), []byte("gate = \"test ! -e broken\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "seed")
	git(t, src, "push", "-q", w.bare, "main")

	w.client = queue.Conn()
	if _, err := w.client.Subscribe(StatusSubjectPrefix+">", func(m *nats.Msg) {
		s := statusMsg{subject: m.Subject}
		if err := json.Unmarshal(m.Data, &s.Status); err != nil {
			t.Error(err)
		}
		// The control node reports pending when it creates a job; the
		// tests here watch what the runner reports.
		if s.State == control.StatePending {
			return
		}
		w.statuses <- s
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

// pushBranch commits content at name on a new branch of the test repo.
func (w *world) pushBranch(t *testing.T, branch, name, content string) {
	t.Helper()
	src := t.TempDir()
	git(t, src, "clone", "-q", w.bare, ".")
	git(t, src, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", branch)
	git(t, src, "push", "-q", "origin", "HEAD:"+branch)
}

// pushConfig commits body as .schmeide/config.toml on the remote default branch.
func (w *world) pushConfig(t *testing.T, body string) {
	t.Helper()
	src := t.TempDir()
	git(t, src, "clone", "-q", w.bare, ".")
	if err := os.WriteFile(filepath.Join(src, ".schmeide", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "config")
	git(t, src, "push", "-q", "origin", "HEAD:main")
}

func (w *world) publish(t *testing.T, steward string, issue int) {
	t.Helper()
	job := control.Job{
		Repo: testRepo, RepoID: testRepoID, Issue: issue, Steward: steward, InstallationID: 1,
		Title: "Fix the Thing!", Body: "The thing is broken.",
	}
	if created, err := w.queue.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("publish job: created=%v err=%v", created, err)
	}
}

// runner builds a runner and does not start it, so a test can call handle.
func (w *world) runner(t *testing.T, steward string, h harness.Harness) (*Runner, string) {
	t.Helper()
	return w.runnerWith(t, steward, h, false)
}

// runnerWith builds a runner and does not start it. pool sets --pool.
func (w *world) runnerWith(t *testing.T, steward string, h harness.Harness, pool bool) (*Runner, string) {
	t.Helper()
	workDir := t.TempDir()
	ackWait := w.ackWait
	if ackWait == 0 {
		ackWait = time.Second
	}
	r, err := New(t.Context(), Options{
		NATSURL:      w.natsURL,
		NATSToken:    testRunnerTokens[steward],
		Steward:      steward,
		Pool:         pool,
		WorkDir:      workDir,
		GitHubAPIURL: w.apiURL,
		GitHubURL:    w.gitRoot,
		GitName:      "schmeide[bot]",
		GitEmail:     "schmeide[bot]@example.test",
		Harnesses:    []harness.Harness{h},
		AckWait:      ackWait,
		StuckAfter:   w.stuckAfter,
		RetryDelay:   10 * time.Millisecond,
		MaxAttempts:  w.maxAttempts,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, workDir
}

// start runs a runner for the steward until the test ends and returns it
// along with its work directory.
func (w *world) start(t *testing.T, steward string, h harness.Harness) (*Runner, string) {
	t.Helper()
	return w.startWith(t, steward, h, false)
}

// startWith is start with --pool when pool is set.
func (w *world) startWith(t *testing.T, steward string, h harness.Harness, pool bool) (*Runner, string) {
	t.Helper()
	r, workDir := w.runnerWith(t, steward, h, pool)
	w.run(t, r)
	return r, workDir
}

// run runs the runner until the test ends.
func (w *world) run(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
		r.Close()
	})
}

// track starts registration tracking, which is what makes an unregistered
// steward's job go to the pool subject.
func (w *world) track(t *testing.T) {
	t.Helper()
	tracker, err := w.queue.Track(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	w.tracker = tracker
}

// waitRegistered waits until the control node has the runner's registration.
func (w *world) waitRegistered(t *testing.T, steward string, pool bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, reg := range w.tracker.Runners() {
			if reg.Steward == steward && reg.Pool == pool {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("runner %s pool=%v did not register; have %+v", steward, pool, w.tracker.Runners())
}

// subjectOf returns the stream subject of the job for issue.
func (w *world) subjectOf(t *testing.T, issue int) string {
	t.Helper()
	js, err := jetstream.New(w.client)
	if err != nil {
		t.Fatal(err)
	}
	st, err := js.Stream(t.Context(), control.JobStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		msg, err := st.GetMsg(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		var job control.Job
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			t.Fatal(err)
		}
		if job.Issue != issue {
			continue
		}
		if found != "" {
			t.Fatalf("issue %d is stored on %s and %s", issue, found, msg.Subject)
		}
		if msg.Subject != job.Subject() {
			t.Fatalf("issue %d stored on %s, payload subject %s", issue, msg.Subject, job.Subject())
		}
		found = msg.Subject
	}
	if found == "" {
		t.Fatalf("issue %d is not in the job stream", issue)
	}
	return found
}

func stewardSubject(steward string, issue int) string {
	return control.Job{RepoID: testRepoID, Issue: issue, Steward: steward}.Subject()
}

func poolSubject(issue int) string {
	return control.Job{RepoID: testRepoID, Issue: issue, Pool: true}.Subject()
}

// nextJob waits for the runner's consumer to deliver one job.
func nextJob(t *testing.T, r *Runner) jetstream.Msg {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		msg, err := r.cons.Next(jetstream.FetchMaxWait(pollWait))
		if err == nil {
			return msg
		}
		last = err
	}
	t.Fatalf("no job arrived: %v", last)
	return nil
}

// take reads statuses until have has n of them.
func (w *world) take(t *testing.T, have []State, n int) []State {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for len(have) < n {
		select {
		case s := <-w.statuses:
			have = append(have, s.State)
		case <-timeout:
			t.Fatalf("saw %v, want %d states", have, n)
		}
	}
	return have
}

// await returns the states reported for the issue, up to and including the
// first final one.
func (w *world) await(t *testing.T, steward string, issue int) []State {
	t.Helper()
	subject := fmt.Sprintf("%s%s.%d.%d", StatusSubjectPrefix, steward, testRepoID, issue)
	return w.awaitSubject(t, subject, issue)
}

// awaitSubject is await for an exact status subject. A pool job reports on
// the pool subject, not the assignee's.
func (w *world) awaitSubject(t *testing.T, subject string, issue int) []State {
	t.Helper()
	var states []State
	timeout := time.After(30 * time.Second)
	for {
		select {
		case s := <-w.statuses:
			if s.subject != subject {
				t.Errorf("status on %q, but only %q has a runner", s.subject, subject)
				continue
			}
			states = append(states, s.State)
			if s.State == StateGateFailed || s.State == StatePROpened || s.State == StateFailed || s.State == StateReviewFailed || s.State == StateMerged || s.State == StateQueued || s.State == StateMergeFailed {
				return states
			}
		case <-timeout:
			t.Fatalf("job for issue %d reached no final state; saw %v", issue, states)
		}
	}
}

// writes returns a fake harness whose session writes the given files, and a
// counter of the sessions it ran.
func writes(files map[string]string) (*harness.Fake, *atomic.Int32) {
	var sessions atomic.Int32
	return &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		sessions.Add(1)
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "pass\n")
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(req.Dir, name), []byte(content), 0o600); err != nil {
				return err
			}
		}
		return nil
	}}, &sessions
}

func TestJobOpensPR(t *testing.T) {
	w := newWorld(t)
	registered := make(chan Registration, 1)
	if _, err := w.client.Subscribe(RegisterSubjectPrefix+"alice", func(m *nats.Msg) {
		var reg Registration
		if err := json.Unmarshal(m.Data, &reg); err != nil {
			t.Error(err)
		}
		registered <- reg
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.client.Flush(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SCHMEIDE_NATS_TOKEN", "runner-secret")
	var prompt, env atomic.Value
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "pass\n")
		}
		prompt.Store(req.Prompt)
		env.Store(req.Env)
		if err := os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("fixed\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.Dir, ".schmeide", "pr.md"), []byte("7: fix the thing\n\nIt was broken.\n"), 0o600)
	}}
	_, workDir := w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	select {
	case reg := <-registered:
		if reg.Steward != "alice" || fmt.Sprint(reg.Harnesses) != "[fake]" || reg.Pool {
			t.Errorf("registration = %+v, want steward alice with harness fake and pool off", reg)
		}
	case <-time.After(5 * time.Second):
		t.Error("runner did not register")
	}
	if p, _ := prompt.Load().(string); !strings.Contains(p, "Fix the Thing!") || !strings.Contains(p, "The thing is broken.") || !strings.Contains(p, "#7") {
		t.Errorf("brief does not carry the issue: %q", p)
	}
	if e, _ := env.Load().([]string); e == nil || slices.ContainsFunc(e, func(v string) bool { return strings.HasPrefix(v, "SCHMEIDE_") }) {
		t.Errorf("session environment = %q, want one without the runner's settings", e)
	}

	const branch = "7_fix-the-thing"
	if got := git(t, w.bare, "show", branch+":fix.txt"); got != "fixed" {
		t.Errorf("pushed fix.txt = %q, want %q", got, "fixed")
	}
	if files := git(t, w.bare, "ls-tree", "-r", "--name-only", branch); strings.Contains(files, "pr.md") {
		t.Errorf("the PR text file was committed: %s", files)
	}
	if author := git(t, w.bare, "log", "-1", "--format=%an", branch); author != "schmeide[bot]" {
		t.Errorf("commit author = %q, want the app's bot", author)
	}

	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want exactly one", pulls)
	}
	want := pull{Number: 1, State: "open", Title: "7: fix the thing", Body: "It was broken.\n\nCloses #7", Head: branch, Base: "main", URL: pulls[0].URL}
	if pulls[0] != want {
		t.Errorf("pull request = %+v, want %+v", pulls[0], want)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want one that links the PR", comments["7"])
	}
	if left, _ := os.ReadDir(filepath.Join(workDir, "worktrees")); len(left) != 0 {
		t.Errorf("worktrees left behind: %v", left)
	}
}

// TestLintGateMismatchCommentsWithoutFailingTheJob adds a Makefile check
// target to a repo whose gate is not make check. The issue comment names the
// detected target and the configured gate, and the job still opens a pull
// request.
func TestLintGateMismatchCommentsWithoutFailingTheJob(t *testing.T) {
	w := newWorld(t)
	src := t.TempDir()
	git(t, src, "clone", "-q", w.bare, ".")
	if err := os.WriteFile(filepath.Join(src, "Makefile"), []byte("check:\n\ttrue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "makefile")
	git(t, src, "push", "-q", "origin", "HEAD:main")

	const gate = "test ! -e broken"
	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing pr_opened]"; got != want {
		t.Fatalf("states = %s, want %s", got, want)
	}
	_, comments := w.gh.Snapshot()
	var warned bool
	for _, body := range comments["7"] {
		if strings.Contains(body, "check") && strings.Contains(body, gate) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("comments = %q, want one that names the detected target check and the configured gate %s", comments["7"], gate)
	}
}

// TestJobLocksGateGitAndHarnessEnvironments runs one job and checks the three
// environments that job starts. The harness session is harnessEnv: the base
// list plus ANTHROPIC_, CLAUDE_, XAI_, and --pass-env, with no SCHMEIDE_ name.
// newWorktree is called without an environment, so git sees only the variables
// newWorktree appends and does not see SCHMEIDE_NATS_TOKEN. The gate command's
// environment stays nil, so the gate inherits the runner process environment,
// including SCHMEIDE_NATS_TOKEN and SCHMEIDE_TEST_GATE_PROOF, which is not on
// the base list. --pass-env does not filter the gate.
func TestJobLocksGateGitAndHarnessEnvironments(t *testing.T) {
	const secret = "gate-sees-this-nats-secret"
	dir := t.TempDir()
	proof := filepath.Join(dir, "gate-proof")
	logPath := filepath.Join(dir, "git-env.log")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("find git: %v", err)
	}
	wrapperDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(wrapperDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// The runner's git process has no PATH. LookPath in this process finds the
	// wrapper; the wrapper execs the real git by absolute path.
	quote := func(s string) string {
		return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
	}
	script := "#!/bin/sh\n" +
		"{ printf '%s\\n' '---'; export -p; } >> " + quote(logPath) + " || exit 1\n" +
		"exec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	w := newWorld(t)
	gate := `printf '%s' "$SCHMEIDE_NATS_TOKEN" > "$SCHMEIDE_TEST_GATE_PROOF"`
	w.pushConfig(t, "gate = "+strconv.Quote(gate)+"\n")

	t.Setenv("SCHMEIDE_NATS_TOKEN", secret)
	t.Setenv("SCHMEIDE_TEST_GATE_PROOF", proof)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var mu sync.Mutex
	var sessions [][]string
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		mu.Lock()
		sessions = append(sessions, slices.Clone(req.Env))
		mu.Unlock()
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "pass\n")
		}
		return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("fixed\n"), 0o600)
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing pr_opened]"; got != want {
		proofBody, _ := os.ReadFile(proof)
		gitLog, _ := os.ReadFile(logPath)
		t.Fatalf("states = %s, want %s\ngate proof = %q\ngit environment:\n%s", got, want, proofBody, gitLog)
	}

	gotProof, err := os.ReadFile(proof)
	if err != nil {
		t.Fatalf("read gate proof: %v", err)
	}
	if string(gotProof) != secret {
		t.Errorf("gate wrote %q, want SCHMEIDE_NATS_TOKEN %q", gotProof, secret)
	}

	gitLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read git environment log: %v", err)
	}
	if !strings.Contains(string(gitLog), "GIT_TERMINAL_PROMPT=") {
		t.Errorf("git environment = %s, want GIT_TERMINAL_PROMPT from newWorktree", gitLog)
	}
	if strings.Contains(string(gitLog), "SCHMEIDE_NATS_TOKEN") || strings.Contains(string(gitLog), secret) {
		t.Errorf("git environment contains SCHMEIDE_NATS_TOKEN:\n%s", gitLog)
	}

	mu.Lock()
	seen := append([][]string(nil), sessions...)
	mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("harness ran no session")
	}
	for _, env := range seen {
		if env == nil || slices.ContainsFunc(env, func(v string) bool { return strings.HasPrefix(v, "SCHMEIDE_") }) {
			t.Errorf("session environment = %q, want one without a SCHMEIDE_ name", env)
		}
		if !slices.ContainsFunc(env, func(v string) bool { return strings.HasPrefix(v, "PATH=") }) {
			t.Errorf("session environment = %q, want the base list, including PATH", env)
		}
	}
}

func TestGateFailureOpensNoPR(t *testing.T) {
	w := newWorld(t)
	h, _ := writes(map[string]string{"broken": "x"})
	_, workDir := w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working gate_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "gate command `test ! -e broken` failed") {
		t.Errorf("issue comments = %q, want one reporting the gate failure", comments["7"])
	}
	if strings.Contains(comments["7"][0], "```") {
		t.Errorf("issue comment %q carries the gate's output", comments["7"][0])
	}
	assertNoMerge(t, w)
	if branches := git(t, w.bare, "branch", "--list", "7_*"); branches != "" {
		t.Errorf("branch pushed despite the gate failure: %s", branches)
	}
	if left, _ := os.ReadDir(filepath.Join(workDir, "worktrees")); len(left) != 0 {
		t.Errorf("worktrees left behind: %v", left)
	}
}

// TestGateTimeoutFailsTheJob uses a short flag so the test does not sleep
// for the 30 minute default. The gate prints a marker and then sleeps; the
// issue comment must say the gate timed out and must not echo that marker.
func TestGateTimeoutFailsTheJob(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, "gate = \"echo DO-NOT-ECHO; sleep 120\"\n")
	h, _ := writes(map[string]string{"fix.txt": "x"})
	r, _ := w.runner(t, "alice", h)
	r.opts.GateTimeout = 200 * time.Millisecond
	w.run(t, r)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working gate_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "the gate timed out") {
		t.Errorf("issue comments = %q, want one saying the gate timed out", comments["7"])
	}
	if len(comments["7"]) == 1 && strings.Contains(comments["7"][0], "DO-NOT-ECHO") {
		t.Errorf("issue comment echoes the gate output: %q", comments["7"][0])
	}
	assertNoMerge(t, w)
	assertSettled(t, w)
}

// TestSessionTimeoutFailsTheJob cancels a harness session that blocks until
// its context is done. The timeout is short so the test does not sleep for
// the 2 hour default.
func TestSessionTimeoutFailsTheJob(t *testing.T) {
	w := newWorld(t)
	w.maxAttempts = 1
	h := &harness.Fake{RunFunc: func(ctx context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "pass\n")
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	r, _ := w.runner(t, "alice", h)
	r.opts.SessionTimeout = 200 * time.Millisecond
	w.run(t, r)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	_, comments := w.gh.Snapshot()
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "session timed out") {
		t.Errorf("issue comments = %q, want one saying the session timed out", comments["7"])
	}
	assertSettled(t, w)
}

func TestDeadlinesFollowConfigThenFlagThenDefault(t *testing.T) {
	r := &Runner{opts: Options{GateTimeout: time.Hour, SessionTimeout: 3 * time.Hour}}
	missing, err := repoconfig.Parse(repoconfig.Path, []byte("gate = \"true\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.deadlinesFor(missing); got.gate != time.Hour || got.session != 3*time.Hour {
		t.Fatalf("missing key selected %+v, want the runner flags", got)
	}
	set, err := repoconfig.Parse(repoconfig.Path, []byte("gate = \"true\"\ngate_timeout = \"30m\"\nsession_timeout = \"30m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.deadlinesFor(set); got.gate != 30*time.Minute || got.session != 30*time.Minute {
		t.Fatalf("config of 30m selected %+v, want 30 minutes", got)
	}
	r.opts.GateTimeout = 0
	r.opts.SessionTimeout = 0
	if got := r.deadlinesFor(missing); got.gate != 30*time.Minute || got.session != 2*time.Hour {
		t.Fatalf("missing key with no flag selected %+v, want 30m and 2h", got)
	}
}

func TestTimeoutFlagDefaults(t *testing.T) {
	fs, o, _, _ := runnerFlags()
	if o.GateTimeout != 30*time.Minute || o.SessionTimeout != 2*time.Hour {
		t.Fatalf("flag defaults are gate %v session %v, want 30m and 2h", o.GateTimeout, o.SessionTimeout)
	}
	if err := ParseFlags(fs, nil); err != nil {
		t.Fatal(err)
	}
	if o.GateTimeout != 30*time.Minute || o.SessionTimeout != 2*time.Hour {
		t.Fatalf("parsed defaults are gate %v session %v, want 30m and 2h", o.GateTimeout, o.SessionTimeout)
	}
	help := Usage()
	if !strings.Contains(help, "--gate-timeout") || !strings.Contains(help, "--session-timeout") || !strings.Contains(help, "default 30m") || !strings.Contains(help, "default 2h") {
		t.Fatalf("help does not document the timeout defaults:\n%s", help)
	}
}

func TestRunnerOnlyTakesItsStewardsJobs(t *testing.T) {
	w := newWorld(t)
	h, sessions := writes(map[string]string{"fix.txt": "x"})
	w.publish(t, "bob", 8)
	w.publish(t, "alice", 7)
	w.start(t, "alice", h)

	// await also fails the test on any status for bob's job.
	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a PR", states)
	}
	// Give a wrongly delivered job time to show up.
	time.Sleep(3 * pollWait)
	if got := sessions.Load(); got != 2 {
		t.Errorf("sessions = %d, want 2: a worker and a reviewer for alice's job only", got)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].Head != "7_fix-the-thing" || len(comments["8"]) != 0 {
		t.Errorf("pulls = %+v, comments = %v; want only alice's issue 7 touched", pulls, comments)
	}
	select {
	case s := <-w.statuses:
		t.Errorf("unexpected status %+v", s)
	default:
	}
}

func TestDeadRunnersJobIsRedeliveredWithoutDuplicatePR(t *testing.T) {
	for _, tt := range []struct {
		name string
		// afterPR kills the first runner right after it opened the PR, before
		// it could acknowledge the job. Otherwise it dies mid-session.
		afterPR      bool
		wantSessions int32
	}{
		{name: "dies mid-session", wantSessions: 2},
		// The second runner resumes the open pull request at its review.
		{name: "dies after opening the PR", afterPR: true, wantSessions: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			var first *Runner
			died := make(chan struct{})
			// A dead runner is one the control node no longer hears from.
			die := sync.OnceFunc(func() {
				first.nc.Close()
				close(died)
			})
			h1, _ := writes(map[string]string{"fix.txt": "x"})
			if tt.afterPR {
				w.gh.OnPR(die)
				// The dead runner never finishes its review.
				work := h1.RunFunc
				h1.RunFunc = func(ctx context.Context, req harness.Request) error {
					if strings.Contains(req.Prompt, reviewLead) {
						<-ctx.Done()
						return ctx.Err()
					}
					return work(ctx, req)
				}
			} else {
				h1.RunFunc = func(ctx context.Context, _ harness.Request) error {
					die()
					<-ctx.Done()
					return ctx.Err()
				}
			}
			first, _ = w.start(t, "alice", h1)
			w.publish(t, "alice", 7)
			select {
			case <-died:
			case <-time.After(30 * time.Second):
				t.Fatal("first runner never got the job")
			}
			for len(w.statuses) > 0 {
				<-w.statuses // what the first runner reported before it died
			}

			h2, sessions := writes(map[string]string{"fix.txt": "x"})
			w.start(t, "alice", h2)
			if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
				t.Fatalf("second runner's states = %v, want a PR", states)
			}
			if got := sessions.Load(); got != tt.wantSessions {
				t.Errorf("second runner ran %d sessions, want %d", got, tt.wantSessions)
			}
			pulls, comments := w.gh.Snapshot()
			if len(pulls) != 1 {
				t.Errorf("pull requests = %+v, want exactly one", pulls)
			}
			if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "review passed") {
				t.Errorf("issue comments = %q, want one outcome comment", comments["7"])
			}
			if got := git(t, w.bare, "show", "7_fix-the-thing:fix.txt"); got != "x" {
				t.Errorf("pushed fix.txt = %q, want %q", got, "x")
			}
		})
	}
}

func TestFailedJobIsRetried(t *testing.T) {
	w := newWorld(t)
	h, sessions := writes(map[string]string{"fix.txt": "x"})
	succeed := h.RunFunc
	h.RunFunc = func(ctx context.Context, req harness.Request) error {
		if sessions.Load() == 0 {
			sessions.Add(1)
			return errors.New("the model was overloaded")
		}
		return succeed(ctx, req)
	}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working retrying claimed working reviewing pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want exactly one", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "opened a pull request") {
		t.Errorf("issue comments = %q, want only the one that links the PR", comments["7"])
	}
}

func TestFailedJobIsFinalAfterMaxAttempts(t *testing.T) {
	w := newWorld(t)
	var sessions atomic.Int32
	h := &harness.Fake{RunFunc: func(context.Context, harness.Request) error {
		sessions.Add(1)
		return errors.New("the model was overloaded")
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	want := "[claimed working retrying claimed working retrying claimed working failed]"
	if got := fmt.Sprint(w.await(t, "alice", 7)); got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	// Give a wrongly redelivered job time to show up.
	time.Sleep(3 * pollWait)
	if got := sessions.Load(); got != defaultMaxAttempts {
		t.Errorf("sessions = %d, want %d: the retries are bounded", got, defaultMaxAttempts)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "the model was overloaded") {
		t.Errorf("issue comments = %q, want one reporting the final failure", comments["7"])
	}
}

func TestAttemptCountSurvivesRunnerRestart(t *testing.T) {
	w := newWorld(t)
	w.track(t)
	w.maxAttempts = 2
	var sessions atomic.Int32
	h := &harness.Fake{RunFunc: func(context.Context, harness.Request) error {
		sessions.Add(1)
		return errors.New("the model was overloaded")
	}}

	first, _ := w.runner(t, "alice", h)
	w.waitRegistered(t, "alice", false)
	w.publish(t, "alice", 7)
	first.handle(t.Context(), nextJob(t, first))
	states := w.take(t, nil, 3)
	if got, want := fmt.Sprint(states), "[claimed working retrying]"; got != want {
		t.Fatalf("first runner states = %s, want %s", got, want)
	}
	first.Close()

	second, _ := w.runner(t, "alice", h)
	second.handle(t.Context(), nextJob(t, second))
	states = w.take(t, states, 6)
	if got, want := fmt.Sprint(states), "[claimed working retrying claimed working failed]"; got != want {
		t.Errorf("states after restart = %s, want %s", got, want)
	}
	if got := sessions.Load(); got != 2 {
		t.Errorf("sessions = %d, want 2: the first runner's failed attempt must survive its restart", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, ok := w.tracker.JobStatus(stewardSubject("alice", 7))
		if ok && status.Attempts == 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	status, ok := w.tracker.JobStatus(stewardSubject("alice", 7))
	t.Errorf("persisted status = %+v, %v; want 2 attempts", status, ok)
}

func TestStuckJobRecovery(t *testing.T) {
	w := newWorld(t)
	w.stuckAfter = time.Second
	entered := make(chan struct{})
	canceled := make(chan struct{})
	hung := &harness.Fake{RunFunc: func(ctx context.Context, _ harness.Request) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	first, _ := w.runner(t, "alice", hung)
	w.publish(t, "alice", 7)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		first.handle(t.Context(), nextJob(t, first))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck runner never started the work")
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck runner's work was not canceled")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck runner did not hand the job back")
	}
	first.Close()

	h, sessions := writes(map[string]string{"fix.txt": "x"})
	second, _ := w.runner(t, "alice", h)
	second.handle(t.Context(), nextJob(t, second))
	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("recovery runner states = %v, want a PR", states)
	}
	if got := sessions.Load(); got != 2 {
		t.Errorf("recovery runner sessions = %d, want worker and reviewer", got)
	}
}

// TestTokenHandBackDoesNotConsumeAttempts is two token refusals and then one
// work failure, with --max-attempts 3. The work failure is still retried.
func TestTokenHandBackDoesNotConsumeAttempts(t *testing.T) {
	w := newWorld(t)
	w.maxAttempts = 3
	var tokenReqs atomic.Int32
	w.mint = func(context.Context, control.Job) (control.Token, error) {
		if tokenReqs.Add(1) <= 2 {
			return control.Token{}, errors.New("github is down")
		}
		return control.Token{Token: testToken}, nil
	}
	h, _ := writes(map[string]string{"fix.txt": "x"})
	succeed := h.RunFunc
	var failedWork atomic.Bool
	h.RunFunc = func(ctx context.Context, req harness.Request) error {
		if !strings.Contains(req.Prompt, reviewLead) && !failedWork.Swap(true) {
			return errors.New("the model was overloaded")
		}
		return succeed(ctx, req)
	}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	// Two token hand-backs, one retried work failure, then the pull request.
	want := []State{
		StateClaimed, StateClaimed, StateClaimed, StateWorking, StateRetrying,
		StateClaimed, StateWorking, StateReviewing, StatePROpened,
	}
	if got := w.await(t, "alice", 7); !slices.Equal(got, want) {
		t.Errorf("states = %v, want %v", got, want)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want one after the retried work failure", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "opened a pull request") {
		t.Errorf("issue comments = %q, want only the pull request comment", comments["7"])
	}
}

// TestShutdownHandBackDoesNotConsumeAttempts is a shutdown Nak and then one
// work failure. MaxAttempts is 2, so that shutdown delivery would already be
// the last try if deliveries were attempts. The work failure is retried.
func TestShutdownHandBackDoesNotConsumeAttempts(t *testing.T) {
	w := newWorld(t)
	w.maxAttempts = 2
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls int
	h := &harness.Fake{RunFunc: func(runCtx context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "pass\n")
		}
		calls++
		switch calls {
		case 1:
			cancel()
			<-runCtx.Done()
			return runCtx.Err()
		case 2:
			return errors.New("the model was overloaded")
		default:
			return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("x\n"), 0o600)
		}
	}}
	r, _ := w.runner(t, "alice", h)
	w.publish(t, "alice", 7)

	r.handle(ctx, nextJob(t, r))
	var states []State
	states = w.take(t, states, 2)
	if got, want := fmt.Sprint(states), "[claimed working]"; got != want {
		t.Fatalf("states after shutdown = %s, want %s", got, want)
	}

	r.handle(t.Context(), nextJob(t, r))
	states = w.take(t, states, 5)
	if states[len(states)-1] != StateRetrying {
		t.Fatalf("states after the work failure = %v, want it retried", states)
	}

	r.handle(t.Context(), nextJob(t, r))
	states = w.take(t, states, 9)
	want := "[claimed working claimed working retrying claimed working reviewing pr_opened]"
	if got := fmt.Sprint(states); got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want one after the retried work failure", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "opened a pull request") {
		t.Errorf("issue comments = %q, want only the pull request comment", comments["7"])
	}
}

func TestJobWithoutIssueTextIsNotWorked(t *testing.T) {
	w := newWorld(t)
	h, sessions := writes(map[string]string{"fix.txt": "x"})
	w.start(t, "alice", h)
	job := control.Job{Repo: testRepo, RepoID: testRepoID, Issue: 7, Steward: "alice", InstallationID: 1}
	if created, err := w.queue.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("publish job: created=%v err=%v", created, err)
	}
	if states := w.await(t, "alice", 7); states[len(states)-1] != StateFailed {
		t.Errorf("states = %v, want the job failed", states)
	}
	if got := sessions.Load(); got != 0 {
		t.Errorf("sessions = %d, want none for a job without the issue's text", got)
	}
}

func TestNewWithWrongTokenSaysWhatToCheck(t *testing.T) {
	w := newWorld(t)
	h, _ := writes(nil)
	_, err := New(t.Context(), Options{NATSURL: w.natsURL, Steward: "alice", NATSToken: testRunnerTokens["bob"], Harnesses: []harness.Harness{h}},
		slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "--runner-tokens lists for alice") {
		t.Errorf("err = %v, want one that points at the steward's runner token", err)
	}
}

func TestEnvKeepsOnlyWhatJobsNeed(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/a", "LC_ALL=C", "SCHMEIDE_NATS_TOKEN=secret", "GITHUB_TOKEN=ghp",
		"ANTHROPIC_API_KEY=sk", "CLAUDE_CONFIG_DIR=/c", "GOPATH=/go", "AWS_SECRET_ACCESS_KEY=aws",
	}
	want := []string{"PATH=/bin", "HOME=/home/a", "LC_ALL=C", "ANTHROPIC_API_KEY=sk", "CLAUDE_CONFIG_DIR=/c"}
	if got := harnessEnv(environ, nil); !slices.Equal(got, want) {
		t.Errorf("harnessEnv = %q, want %q", got, want)
	}
}

func TestNewWithoutControlNodeSaysWhatToCheck(t *testing.T) {
	h, _ := writes(nil)
	_, err := New(t.Context(), Options{NATSURL: "nats://127.0.0.1:1", Steward: "alice", Harnesses: []harness.Harness{h}},
		slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "Check that `schmeide control` is running") {
		t.Errorf("err = %v, want one that says what to check", err)
	}
}

func TestNewWithoutUsableHarnessSaysHowToFix(t *testing.T) {
	w := newWorld(t)
	h := &harness.Fake{CheckErr: fmt.Errorf("%w: run `fake login`", harness.ErrNotLoggedIn)}
	_, err := New(t.Context(), Options{NATSURL: w.natsURL, Steward: "alice", NATSToken: testRunnerTokens["alice"], Harnesses: []harness.Harness{h}},
		slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "run `fake login`") || !strings.Contains(err.Error(), "schmeide doctor") {
		t.Errorf("err = %v, want the harness's fix and a pointer to doctor", err)
	}
}

func TestBranchName(t *testing.T) {
	for title, want := range map[string]string{
		"Fix the Thing!": "12_fix-the-thing",
		"":               "12_issue",
		"add runner that takes a job from issue to opened pr": "12_add-runner-that-takes-a-job-from-issue-t",
	} {
		if got := BranchName(12, title); got != want {
			t.Errorf("BranchName(12, %q) = %q, want %q", title, got, want)
		}
	}
}

func writeVerdict(req harness.Request, body string) error {
	return os.WriteFile(filepath.Join(req.Dir, filepath.FromSlash(reviewTextPath)), []byte(body), 0o600)
}

func assertNoMerge(t *testing.T, w *world) {
	t.Helper()
	for _, call := range w.gh.Calls() {
		if strings.Contains(strings.ToLower(call), "merge") {
			t.Errorf("merge was attempted: %s", call)
		}
	}
}

// assertSettled fails if another status arrives, which is what a retried job
// would publish.
func assertSettled(t *testing.T, w *world) {
	t.Helper()
	time.Sleep(3 * pollWait)
	select {
	case s := <-w.statuses:
		t.Errorf("unexpected status after the job finished: %+v", s)
	default:
	}
}

const testGateConfig = "gate = \"test ! -e broken\"\n"

// testMergeConfig turns factory merge on and leaves review fixes off.
const testMergeConfig = testGateConfig + "[automate]\nmerge = \"on\"\n"

// testReviewFixConfig turns review fix rounds on and leaves merge off.
const testReviewFixConfig = testGateConfig + "[automate]\nreview_fix = \"on\"\n"

func TestFactoryMergeWhenChecksSucceed(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig)
	w.gh.SetChecks([]fakegh.CheckRun{
		{Name: "test", Status: "completed", Conclusion: "success"},
		{Name: "lint", Status: "completed", Conclusion: "success"},
	})
	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing merged]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	merges, method := w.gh.Merges()
	if merges != 1 || method != "merge" {
		t.Errorf("merges = %d method %q, want one merge commit", merges, method)
	}
	sha := git(t, w.bare, "rev-parse", "7_fix-the-thing")
	sawCheck := false
	for _, call := range w.gh.Calls() {
		if strings.Contains(call, "/commits/"+sha+"/check-runs") {
			sawCheck = true
		}
	}
	if !sawCheck {
		t.Errorf("check runs were not read for %s: %v", sha, w.gh.Calls())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want the one that was merged", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "merged") || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want one that says the pull request was merged", comments["7"])
	}
}

func TestFactoryMergeWhenACheckFails(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig)
	w.gh.SetChecks([]fakegh.CheckRun{
		{Name: "test", Status: "completed", Conclusion: "success"},
		{Name: "lint", Status: "completed", Conclusion: "failure"},
	})
	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing merge_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if merges, _ := w.gh.Merges(); merges != 0 {
		t.Errorf("merges = %d, want none", merges)
	}
	assertNoMerge(t, w)
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "left the pull request open") || !strings.Contains(comments["7"][0], "failure") || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want the failed check and the open pull request", comments["7"])
	}
}

func TestFactoryMergeRefusedByRequiredReviews(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig)
	w.gh.SetChecks([]fakegh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}})
	w.gh.Protect("main", 1)
	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing merge_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	assertNoMerge(t, w)
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].State != "open" {
		t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "approving review") || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want the required review named", comments["7"])
	}
}

// TestFactoryMergeRefusedByGitHub has green checks but a merge GitHub
// refuses, which must fail the job rather than report it merged.
func TestFactoryMergeRefusedByGitHub(t *testing.T) {
	for _, tt := range []struct {
		status int
		says   string
	}{
		{http.StatusMethodNotAllowed, "Pull Request is not mergeable"},
		{http.StatusConflict, "Head branch was modified"},
	} {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			w := newWorld(t)
			w.pushConfig(t, testMergeConfig)
			w.gh.SetChecks([]fakegh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}})
			w.gh.FailMerge(tt.status)
			h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
			w.start(t, "alice", h)
			w.publish(t, "alice", 7)

			if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing merge_failed]"; got != want {
				t.Errorf("states = %s, want %s", got, want)
			}
			assertSettled(t, w)
			if merges, _ := w.gh.Merges(); merges != 0 {
				t.Errorf("merges = %d, want none", merges)
			}
			pulls, comments := w.gh.Snapshot()
			if len(pulls) != 1 || pulls[0].State != "open" {
				t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
			}
			if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "left the pull request open") || !strings.Contains(comments["7"][0], strconv.Itoa(tt.status)) || !strings.Contains(comments["7"][0], tt.says) {
				t.Errorf("issue comments = %q, want a failure naming GitHub's %d", comments["7"], tt.status)
			}
		})
	}
}

// The repo's check_wait bounds the poll. The default of 2m would outlast the
// test's timeout.
func TestFactoryMergeWithNoCheckRunsDoesNotMerge(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testGateConfig+"check_wait = \"20ms\"\n[automate]\nmerge = \"on\"\n")
	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing merge_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	assertNoMerge(t, w)
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "no check runs") || !strings.Contains(comments["7"][0], "left the pull request open") {
		t.Errorf("issue comments = %q, want one that says there are no check runs", comments["7"])
	}
}

func TestAutomateMergeNopeIsAConfigError(t *testing.T) {
	w := newWorld(t)
	w.maxAttempts = 1
	w.pushConfig(t, testGateConfig+"[automate]\nmerge = \"nope\"\n")
	h, sessions := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if sessions.Load() != 0 {
		t.Errorf("sessions = %d, want none for a config error", sessions.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
	assertNoMerge(t, w)
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "nope") || !strings.Contains(comments["7"][0], `automate.merge = "on"`) || !strings.Contains(comments["7"][0], `automate.merge = "off"`) || !strings.Contains(comments["7"][0], "did not open a pull request") {
		t.Errorf("issue comments = %q, want a config error that names on and off", comments["7"])
	}
}

func TestJudgeChecks(t *testing.T) {
	if verdict, _ := judgeChecks([]ciwatch.Run{{Name: "test", Status: "completed", Conclusion: "success"}}); verdict != checksGreen {
		t.Fatalf("one success = %v, want green", verdict)
	}
	verdict, why := judgeChecks(nil)
	if verdict != checksPending || !strings.Contains(why, "no check runs") {
		t.Fatalf("no runs = %v %q, want pending", verdict, why)
	}
	verdict, why = judgeChecks([]ciwatch.Run{
		{Name: "test", Status: "completed", Conclusion: "success"},
		{Name: "lint", Status: "completed", Conclusion: "failure"},
	})
	if verdict != checksRed || !strings.Contains(why, "failure") || !strings.Contains(why, "lint") {
		t.Fatalf("mixed = %v %q, want the failure", verdict, why)
	}
	verdict, why = judgeChecks([]ciwatch.Run{
		{Name: "test", Status: "completed", Conclusion: "failure"},
		{Name: "lint", Status: "in_progress"},
	})
	if verdict != checksRed || !strings.Contains(why, "failure") {
		t.Fatalf("failed plus pending = %v %q, want red without waiting", verdict, why)
	}
	verdict, _ = judgeChecks([]ciwatch.Run{{Name: "test", Status: "in_progress"}})
	if verdict != checksPending {
		t.Fatalf("in progress = %v, want pending", verdict)
	}
}

func TestRunnerHelpDocumentsReviewRounds(t *testing.T) {
	got := Usage()
	if !strings.Contains(got, "--max-review-rounds") || !strings.Contains(got, `default "3"`) || !strings.Contains(got, "leaves the pull request open") || !strings.Contains(got, `automate.review_fix = "on"`) {
		t.Fatalf("help does not document the review round limit:\n%s", got)
	}
	if strings.Contains(got, "never merges") || strings.Contains(got, "does not merge") {
		t.Fatalf("help still says the job does not merge:\n%s", got)
	}
	if !strings.Contains(got, `automate.merge = "on"`) || !strings.Contains(got, "The default is a person.") {
		t.Fatalf("help does not document factory merge:\n%s", got)
	}
	if !strings.Contains(got, `check_wait = "10m"`) || !strings.Contains(got, "default 2m") {
		t.Fatalf("help does not document check_wait:\n%s", got)
	}
}

func TestReadVerdict(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".schmeide"), 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, filepath.FromSlash(reviewTextPath))
	if _, _, err := readVerdict(dir); err == nil {
		t.Fatal("missing verdict: want an error")
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("pass\n")
	kind, findings, err := readVerdict(dir)
	if err != nil || kind != verdictPass || findings != "" {
		t.Fatalf("pass = %v, %q, %v", kind, findings, err)
	}
	if _, statErr := os.Stat(file); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("verdict file still present: %v", statErr)
	}
	write("changes\nadd a test\n")
	kind, findings, err = readVerdict(dir)
	if err != nil || kind != verdictChanges || findings != "add a test" {
		t.Fatalf("changes = %v, %q, %v", kind, findings, err)
	}
	for _, body := range []string{"changes\n", "pass\nalso this\n", "nope\n"} {
		write(body)
		if _, _, err := readVerdict(dir); err == nil {
			t.Errorf("verdict %q: want an error", body)
		}
	}
}

func TestReviewPassesOnFirstRound(t *testing.T) {
	w := newWorld(t)
	var workers, reviews atomic.Int32
	var reviewPrompt atomic.Value
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			reviews.Add(1)
			reviewPrompt.Store(req.Prompt)
			return writeVerdict(req, "pass\n")
		}
		if workers.Add(1) != 1 {
			return errors.New("extra worker session")
		}
		return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("fixed\n"), 0o600)
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if workers.Load() != 1 || reviews.Load() != 1 {
		t.Errorf("workers = %d, reviews = %d, want one worker and one reviewer", workers.Load(), reviews.Load())
	}
	prompt, _ := reviewPrompt.Load().(string)
	for _, part := range []string{"Fix the Thing!", "The thing is broken.", "#7", reviewTextPath, "pass", "changes"} {
		if !strings.Contains(prompt, part) {
			t.Errorf("review brief missing %q: %s", part, prompt)
		}
	}
	if files := git(t, w.bare, "ls-tree", "-r", "--name-only", "7_fix-the-thing"); strings.Contains(files, "review.md") {
		t.Errorf("the verdict file was committed: %s", files)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].Head != "7_fix-the-thing" {
		t.Fatalf("pull requests = %+v, want one on the job branch", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], pulls[0].URL) || !strings.Contains(comments["7"][0], "The review passed.") {
		t.Errorf("issue comments = %q, want one that links the PR and says the review passed", comments["7"])
	}
	assertNoMerge(t, w)
}

func TestReviewChangesThenPassUpdatesSameBranch(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testReviewFixConfig)
	const findings = "set fix.txt to reviewed"
	var workers, reviews atomic.Int32
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			if reviews.Add(1) == 1 {
				return writeVerdict(req, "changes\n"+findings+"\n")
			}
			return writeVerdict(req, "pass\n")
		}
		switch workers.Add(1) {
		case 1:
			return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("first\n"), 0o600)
		case 2:
			if !strings.Contains(req.Prompt, findings) || !strings.Contains(req.Prompt, "Fix the Thing!") {
				return fmt.Errorf("fix brief missing the findings or the issue: %s", req.Prompt)
			}
			return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("reviewed\n"), 0o600)
		default:
			return errors.New("extra worker session")
		}
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing working reviewing pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	if workers.Load() != 2 || reviews.Load() != 2 {
		t.Errorf("workers = %d, reviews = %d, want one fix and a second review", workers.Load(), reviews.Load())
	}
	const branch = "7_fix-the-thing"
	if got := git(t, w.bare, "show", branch+":fix.txt"); got != "reviewed" {
		t.Errorf("pushed fix.txt = %q, want the reviewed content", got)
	}
	if got := git(t, w.bare, "rev-list", "--count", "main.."+branch); got != "2" {
		t.Errorf("commits on %s = %s, want 2", branch, got)
	}
	if tip := git(t, w.bare, "log", "-1", "--format=%s", branch); tip != "Apply review feedback for #7" {
		t.Errorf("tip commit = %q, want the review fix", tip)
	}
	pulls, _ := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].Head != branch {
		t.Errorf("pull requests = %+v, want the same branch updated, not a second pull request", pulls)
	}
	assertNoMerge(t, w)
}

func TestReviewExhaustingRoundsFailsAndDoesNotMerge(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testReviewFixConfig)
	var workers, reviews atomic.Int32
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			reviews.Add(1)
			return writeVerdict(req, "changes\nstill wrong\n")
		}
		n := workers.Add(1)
		return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte(fmt.Sprintf("attempt %d\n", n)), 0o600)
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	want := "[claimed working reviewing working reviewing working reviewing review_failed]"
	if got := fmt.Sprint(w.await(t, "alice", 7)); got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if reviews.Load() != int32(defaultMaxReviewRounds) || workers.Load() != int32(defaultMaxReviewRounds) {
		t.Errorf("workers = %d, reviews = %d, want %d of each: a fix follows every rejection except the last", workers.Load(), reviews.Load(), defaultMaxReviewRounds)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want the original pull request left open", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "failed") || !strings.Contains(comments["7"][0], "left the pull request open") || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want one failure comment that leaves the pull request open", comments["7"])
	}
	if got := git(t, w.bare, "show", "7_fix-the-thing:fix.txt"); got != "attempt 3" {
		t.Errorf("pushed fix.txt = %q, want the last fix that was pushed", got)
	}
	assertNoMerge(t, w)
}

func TestReviewFixGateFailureStopsTheReview(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testReviewFixConfig)
	var workers, reviews atomic.Int32
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			if reviews.Add(1) > 1 {
				return errors.New("review continued after the gate failed")
			}
			return writeVerdict(req, "changes\ncreate a file named broken\n")
		}
		if workers.Add(1) == 1 {
			return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("ok\n"), 0o600)
		}
		return os.WriteFile(filepath.Join(req.Dir, "broken"), []byte("x\n"), 0o600)
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing working gate_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if workers.Load() != 2 || reviews.Load() != 1 {
		t.Errorf("workers = %d, reviews = %d, want no review after the gate failure", workers.Load(), reviews.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
	}
	comment := ""
	if len(comments["7"]) == 1 {
		comment = comments["7"][0]
	}
	if !strings.Contains(comment, "gate command `test ! -e broken` failed") || !strings.Contains(comment, "left the pull request open") || !strings.Contains(comment, pulls[0].URL) {
		t.Errorf("issue comments = %q, want the gate failure and the open pull request", comments["7"])
	}
	if strings.Contains(comment, "```") || strings.Contains(comment, "did not open a pull request") {
		t.Errorf("issue comment = %q, want no gate output and no claim that the pull request was not opened", comment)
	}
	if got := git(t, w.bare, "show", "7_fix-the-thing:fix.txt"); got != "ok" {
		t.Errorf("pushed fix.txt = %q, want the content from before the failing fix", got)
	}
	if files := git(t, w.bare, "ls-tree", "-r", "--name-only", "7_fix-the-thing"); strings.Contains(files, "broken") {
		t.Errorf("the failing fix was pushed: %s", files)
	}
	assertNoMerge(t, w)
}

// rejectingHarness writes fix.txt in every worker session and requests
// changes in every review.
func rejectingHarness() (*harness.Fake, *atomic.Int32, *atomic.Int32) {
	var workers, reviews atomic.Int32
	return &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			reviews.Add(1)
			return writeVerdict(req, "changes\nstill wrong\n")
		}
		n := workers.Add(1)
		return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte(fmt.Sprintf("attempt %d\n", n)), 0o600)
	}}, &workers, &reviews
}

// TestReviewFixOffLeavesARejectedPullRequestOpen has no [automate] table, so
// a review that requests changes gets no fix round.
func TestReviewFixOffLeavesARejectedPullRequestOpen(t *testing.T) {
	w := newWorld(t)
	h, workers, reviews := rejectingHarness()
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing review_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if workers.Load() != 1 || reviews.Load() != 1 {
		t.Errorf("workers = %d, reviews = %d, want one worker, one review, and no fix round", workers.Load(), reviews.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].State != "open" {
		t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "requested changes") || !strings.Contains(comments["7"][0], "automate.review_fix") || !strings.Contains(comments["7"][0], "left the pull request open") || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want one that the review requested changes and review_fix is off", comments["7"])
	}
	if got := git(t, w.bare, "rev-list", "--count", "main..7_fix-the-thing"); got != "1" {
		t.Errorf("commits on the job branch = %s, want only the first", got)
	}
	assertNoMerge(t, w)
}

// TestReviewFixOffWithMergeOnDoesNotMergeARejection is factory merge without
// review fixes: merge still waits for a passing review.
func TestReviewFixOffWithMergeOnDoesNotMergeARejection(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testMergeConfig)
	w.gh.SetChecks([]fakegh.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}})
	h, workers, reviews := rejectingHarness()
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing review_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if workers.Load() != 1 || reviews.Load() != 1 {
		t.Errorf("workers = %d, reviews = %d, want no fix round", workers.Load(), reviews.Load())
	}
	if pulls, _ := w.gh.Snapshot(); len(pulls) != 1 || pulls[0].State != "open" {
		t.Errorf("pull requests = %+v, want the pull request left open", pulls)
	}
	assertNoMerge(t, w)
}

func TestReviewerSessionFailureIsFinal(t *testing.T) {
	w := newWorld(t)
	var workers, reviews atomic.Int32
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			reviews.Add(1)
			return errors.New("the reviewer crashed")
		}
		workers.Add(1)
		return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("x\n"), 0o600)
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing review_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	assertSettled(t, w)
	if workers.Load() != 1 || reviews.Load() != 1 {
		t.Errorf("workers = %d, reviews = %d, want no retry and no fix session", workers.Load(), reviews.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want the pull request left open", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "the reviewer crashed") || !strings.Contains(comments["7"][0], "left the pull request open") || !strings.Contains(comments["7"][0], pulls[0].URL) {
		t.Errorf("issue comments = %q, want the reviewer failure and the open pull request", comments["7"])
	}
	assertNoMerge(t, w)
}

// TestForeignJobBranchCommitIsKept is a remote commit on the job branch that
// this runner did not create. The push must leave it there and fail the job.
func TestForeignJobBranchCommitIsKept(t *testing.T) {
	w := newWorld(t)
	const branch = "7_fix-the-thing"
	src := t.TempDir()
	git(t, src, "clone", "-q", w.bare, ".")
	git(t, src, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(src, "theirs.txt"), []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "-c", "user.name=someone", "-c", "user.email=someone@example.test", "commit", "-q", "-m", "foreign")
	git(t, src, "push", "-q", "origin", "HEAD:"+branch)
	foreign := git(t, w.bare, "rev-parse", branch)

	h, _ := writes(map[string]string{"fix.txt": "x"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StateFailed {
		t.Fatalf("states = %v, want the job failed", states)
	}
	if got := git(t, w.bare, "rev-parse", branch); got != foreign {
		t.Fatalf("job branch is %s, want the foreign commit %s kept", got, foreign)
	}
	if got := git(t, w.bare, "show", branch+":theirs.txt"); got != "not ours" {
		t.Errorf("foreign file = %q, want it kept", got)
	}
	if files := git(t, w.bare, "ls-tree", "-r", "--name-only", branch); strings.Contains(files, "fix.txt") {
		t.Errorf("the runner's commit was pushed: %s", files)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
	comment := ""
	if len(comments["7"]) == 1 {
		comment = comments["7"][0]
	}
	if !strings.Contains(comment, "moved") || !strings.Contains(comment, "look at that branch") {
		t.Errorf("issue comments = %q, want one saying the branch moved and that someone has to look at that branch", comments["7"])
	}
}

func TestPoolNameIsNotASteward(t *testing.T) {
	_, err := New(t.Context(), Options{Steward: control.PoolName}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), control.PoolName) {
		t.Fatalf("New(%q) = %v, want a refusal of that steward", control.PoolName, err)
	}
}

func TestRegisteredStewardKeepsTheJobFromThePool(t *testing.T) {
	w := newWorld(t)
	w.track(t)
	own, ownSessions := writes(map[string]string{"fix.txt": "own"})
	pool, poolSessions := writes(map[string]string{"fix.txt": "pool"})
	w.start(t, "alice", own)
	w.startWith(t, "bob", pool, true)
	w.waitRegistered(t, "alice", false)
	w.waitRegistered(t, "bob", true)

	w.publish(t, "alice", 7)
	if got, want := w.subjectOf(t, 7), stewardSubject("alice", 7); got != want {
		t.Fatalf("subject = %s, want %s", got, want)
	}
	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a PR", states)
	}
	time.Sleep(3 * pollWait)
	if got := poolSessions.Load(); got != 0 {
		t.Errorf("pool runner sessions = %d, want 0", got)
	}
	if got := ownSessions.Load(); got != 2 {
		t.Errorf("steward sessions = %d, want 2", got)
	}
}

func TestUnregisteredStewardIsWorkedByAPoolRunner(t *testing.T) {
	w := newWorld(t)
	w.track(t)
	h, sessions := writes(map[string]string{"fix.txt": "x"})
	w.startWith(t, "bob", h, true)
	w.waitRegistered(t, "bob", true)

	w.publish(t, "carol", 8)
	if got, want := w.subjectOf(t, 8), poolSubject(8); got != want {
		t.Fatalf("subject = %s, want %s", got, want)
	}
	subject := fmt.Sprintf("%s%s.%d.%d", StatusSubjectPrefix, control.PoolName, testRepoID, 8)
	if states := w.awaitSubject(t, subject, 8); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a PR", states)
	}
	if got := sessions.Load(); got != 2 {
		t.Errorf("sessions = %d, want 2", got)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].Head != "8_fix-the-thing" || len(comments["8"]) != 1 {
		t.Errorf("pulls = %+v, comments = %v; want carol's issue worked like any other job", pulls, comments)
	}
}

func TestRunnerWithoutPoolLeavesThePoolJob(t *testing.T) {
	w := newWorld(t)
	w.track(t)
	h, sessions := writes(map[string]string{"fix.txt": "x"})
	w.start(t, "alice", h)
	w.waitRegistered(t, "alice", false)

	w.publish(t, "carol", 8)
	w.publish(t, "alice", 7)
	if got, want := w.subjectOf(t, 8), poolSubject(8); got != want {
		t.Fatalf("subject = %s, want %s", got, want)
	}
	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a PR", states)
	}
	time.Sleep(3 * pollWait)
	if got := sessions.Load(); got != 2 {
		t.Errorf("sessions = %d, want 2 for alice's job only", got)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].Head != "7_fix-the-thing" || len(comments["8"]) != 0 {
		t.Errorf("pulls = %+v, comments = %v; want only alice's issue touched", pulls, comments)
	}
	select {
	case s := <-w.statuses:
		t.Errorf("unexpected status %+v", s)
	default:
	}
}

func TestPoolRunnerWorksItsOwnJobFirst(t *testing.T) {
	w := newWorld(t)
	w.track(t)
	h, sessions := writes(map[string]string{"fix.txt": "x"})
	r, _ := w.runnerWith(t, "alice", h, true)
	w.waitRegistered(t, "alice", true)
	w.publish(t, "carol", 8)
	w.publish(t, "alice", 7)
	if got, want := w.subjectOf(t, 8), poolSubject(8); got != want {
		t.Fatalf("pool subject = %s, want %s", got, want)
	}
	if got, want := w.subjectOf(t, 7), stewardSubject("alice", 7); got != want {
		t.Fatalf("own subject = %s, want %s", got, want)
	}

	w.run(t, r)
	if states := w.await(t, "alice", 7); states[0] != StateClaimed || states[len(states)-1] != StatePROpened {
		t.Fatalf("own states = %v, want alice's job to finish first", states)
	}
	subject := fmt.Sprintf("%s%s.%d.%d", StatusSubjectPrefix, control.PoolName, testRepoID, 8)
	if states := w.awaitSubject(t, subject, 8); states[len(states)-1] != StatePROpened {
		t.Fatalf("pool states = %v, want a PR", states)
	}
	if got := sessions.Load(); got != 4 {
		t.Errorf("sessions = %d, want 4: worker and reviewer for each job", got)
	}
}

func TestPoolRunnersShareOneJob(t *testing.T) {
	w := newWorld(t)
	w.track(t)
	h1, s1 := writes(map[string]string{"fix.txt": "a"})
	h2, s2 := writes(map[string]string{"fix.txt": "b"})
	w.startWith(t, "alice", h1, true)
	w.startWith(t, "bob", h2, true)
	w.waitRegistered(t, "alice", true)
	w.waitRegistered(t, "bob", true)

	w.publish(t, "carol", 9)
	subject := fmt.Sprintf("%s%s.%d.%d", StatusSubjectPrefix, control.PoolName, testRepoID, 9)
	if states := w.awaitSubject(t, subject, 9); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a PR", states)
	}
	time.Sleep(3 * pollWait)
	if got := s1.Load() + s2.Load(); got != 2 {
		t.Errorf("sessions = %d + %d, want 2 between the two pool runners", s1.Load(), s2.Load())
	}
	pulls, _ := w.gh.Snapshot()
	if len(pulls) != 1 || pulls[0].Head != "9_fix-the-thing" {
		t.Errorf("pulls = %+v, want exactly one", pulls)
	}
}

func assertBlockedBeforeOpen(t *testing.T, w *world, issue int) {
	t.Helper()
	blocked, opened := -1, -1
	needle := fmt.Sprintf("GET /repos/%s/issues/%d/dependencies/blocked_by", testRepo, issue)
	open := "POST /repos/" + testRepo + "/pulls"
	for i, call := range w.gh.Calls() {
		if blocked < 0 && call == needle {
			blocked = i
		}
		if opened < 0 && call == open {
			opened = i
		}
	}
	if blocked < 0 {
		t.Errorf("GitHub was not asked which issues block #%d: %v", issue, w.gh.Calls())
	}
	if opened >= 0 && blocked > opened {
		t.Errorf("blocked-by was requested after the pull request was opened: %v", w.gh.Calls())
	}
}

func pullByHead(pulls []pull, head string) (pull, int) {
	found := pull{}
	n := 0
	for _, p := range pulls {
		if p.Head == head {
			found = p
			n++
		}
	}
	return found, n
}

func assertCommitParent(t *testing.T, bare, branch, parent string) {
	t.Helper()
	got := git(t, bare, "rev-parse", branch+"^")
	want := git(t, bare, "rev-parse", parent)
	if got != want {
		t.Errorf("parent of %s is %s, want the tip of %s (%s)", branch, got, parent, want)
	}
}

func assertNoBlob(t *testing.T, bare, rev, name string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "cat-file", "-e", rev+":"+name)
	cmd.Dir = bare
	if err := cmd.Run(); err == nil {
		t.Errorf("%s contains %s", rev, name)
	}
}

func TestNoDependencyOpensAgainstTheDefaultBranch(t *testing.T) {
	const branch = "7_fix-the-thing"
	w := newWorld(t)
	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a pull request", states)
	}
	assertBlockedBeforeOpen(t, w, 7)
	pulls, _ := w.gh.Snapshot()
	got, n := pullByHead(pulls, branch)
	if n != 1 {
		t.Fatalf("pulls = %+v, want one %s", pulls, branch)
	}
	if got.Base != "main" {
		t.Errorf("base = %q, want main", got.Base)
	}
	assertCommitParent(t, w.bare, branch, "main")
}

func TestOneOpenDependencyStacksThePullRequest(t *testing.T) {
	const (
		dep    = "3_lay-the-groundwork"
		branch = "7_fix-the-thing"
	)
	w := newWorld(t)
	w.pushBranch(t, dep, "stacked.txt", "from-3\n")
	w.gh.AddBlocker(7, fakegh.Issue{Number: 3, State: "open"})
	w.gh.SeedPull(pull{Head: dep, Base: "main", State: "open"})

	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a pull request", states)
	}
	assertBlockedBeforeOpen(t, w, 7)
	pulls, _ := w.gh.Snapshot()
	got, n := pullByHead(pulls, branch)
	if n != 1 {
		t.Fatalf("pulls = %+v, want one %s", pulls, branch)
	}
	if got.Base != dep {
		t.Errorf("base = %q, want %s", got.Base, dep)
	}
	assertCommitParent(t, w.bare, branch, dep)
	if file := git(t, w.bare, "show", branch+":stacked.txt"); file != "from-3" {
		t.Errorf("stacked.txt = %q, want the dependency branch's file", file)
	}
	assertNoBlob(t, w.bare, "main", "stacked.txt")
}

func TestMergedDependencyOpensAgainstTheDefaultBranch(t *testing.T) {
	const (
		dep    = "3_already-merged"
		branch = "7_fix-the-thing"
	)
	w := newWorld(t)
	w.pushBranch(t, dep, "stacked.txt", "from-3\n")
	w.gh.AddBlocker(7, fakegh.Issue{Number: 3, State: "closed"})
	w.gh.SeedPull(pull{Head: dep, Base: "main", State: "closed"})

	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a pull request on the default branch", states)
	}
	assertBlockedBeforeOpen(t, w, 7)
	pulls, _ := w.gh.Snapshot()
	got, n := pullByHead(pulls, branch)
	if n != 1 {
		t.Fatalf("pulls = %+v, want one %s", pulls, branch)
	}
	if got.Base != "main" {
		t.Errorf("base = %q, want main", got.Base)
	}
	assertCommitParent(t, w.bare, branch, "main")
	assertNoBlob(t, w.bare, branch, "stacked.txt")
}

func TestLinearChainOfDependenciesStacksOnTheTop(t *testing.T) {
	const (
		bottom = "3_first-part"
		top    = "4_second-part"
		branch = "7_fix-the-thing"
	)
	w := newWorld(t)
	w.pushBranch(t, bottom, "stacked.txt", "from-3\n")
	src := t.TempDir()
	git(t, src, "clone", "-q", "-b", bottom, w.bare, ".")
	git(t, src, "checkout", "-q", "-b", top)
	if err := os.WriteFile(filepath.Join(src, "second.txt"), []byte("from-4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", top)
	git(t, src, "push", "-q", "origin", "HEAD:"+top)
	// The top of the chain is listed first, so the order of blockers does
	// not decide which one is the top.
	w.gh.AddBlocker(7, fakegh.Issue{Number: 4, State: "open"})
	w.gh.AddBlocker(7, fakegh.Issue{Number: 3, State: "open"})
	w.gh.SeedPull(pull{Head: bottom, Base: "main", State: "open"})
	w.gh.SeedPull(pull{Head: top, Base: bottom, State: "open"})

	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a pull request stacked on the chain", states)
	}
	pulls, _ := w.gh.Snapshot()
	got, n := pullByHead(pulls, branch)
	if n != 1 {
		t.Fatalf("pulls = %+v, want one %s", pulls, branch)
	}
	if got.Base != top {
		t.Errorf("base = %q, want the top of the chain, %s", got.Base, top)
	}
	assertCommitParent(t, w.bare, branch, top)
}

func TestTwoSideBySideDependenciesOpenNothing(t *testing.T) {
	w := newWorld(t)
	w.pushBranch(t, "3_first-part", "stacked.txt", "from-3\n")
	w.pushBranch(t, "4_second-part", "stacked.txt", "from-4\n")
	w.gh.AddBlocker(7, fakegh.Issue{Number: 3, State: "open"})
	w.gh.AddBlocker(7, fakegh.Issue{Number: 4, State: "open"})
	w.gh.SeedPull(pull{Head: "3_first-part", Base: "main", State: "open"})
	w.gh.SeedPull(pull{Head: "4_second-part", Base: "main", State: "open"})

	h, sessions := writes(map[string]string{"fix.txt": "x"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	states := w.await(t, "alice", 7)
	if states[len(states)-1] != StateFailed {
		t.Fatalf("states = %v, want the job failed", states)
	}
	for _, state := range states {
		if state == StateWorking || state == StatePROpened {
			t.Errorf("job went ahead: %v", states)
			break
		}
	}
	assertBlockedBeforeOpen(t, w, 7)
	if got := sessions.Load(); got != 0 {
		t.Errorf("sessions = %d, want none", got)
	}
	pulls, comments := w.gh.Snapshot()
	if _, n := pullByHead(pulls, "7_fix-the-thing"); n != 0 {
		t.Errorf("pulls = %+v, want no pull request for the job", pulls)
	}
	if branches := git(t, w.bare, "branch", "--list", "7_*"); branches != "" {
		t.Errorf("branch pushed: %s", branches)
	}
	comment := ""
	if len(comments["7"]) == 1 {
		comment = comments["7"][0]
	}
	if !strings.Contains(comment, "not one linear stack") || !strings.Contains(comment, "did not open a pull request") {
		t.Errorf("issue comments = %q, want one that says the dependencies are not one linear stack and that no pull request was opened", comments["7"])
	}
}

func TestBlockerWithoutSchmeidePullRequestUsesDefaultBranch(t *testing.T) {
	const branch = "7_fix-the-thing"
	w := newWorld(t)
	w.pushBranch(t, "wip", "stacked.txt", "from-wip\n")
	w.gh.AddBlocker(7, fakegh.Issue{Number: 3, State: "open"})
	w.gh.SeedPull(pull{Head: "wip", Base: "main", State: "open"})

	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a pull request on the default branch", states)
	}
	pulls, _ := w.gh.Snapshot()
	got, n := pullByHead(pulls, branch)
	if n != 1 {
		t.Fatalf("pulls = %+v, want one %s", pulls, branch)
	}
	if got.Base != "main" {
		t.Errorf("base = %q, want main", got.Base)
	}
	assertCommitParent(t, w.bare, branch, "main")
	assertNoBlob(t, w.bare, branch, "stacked.txt")
}

func TestOneStackableDependencyIgnoresAnUnstackableBlocker(t *testing.T) {
	const (
		dep    = "3_lay-the-groundwork"
		branch = "7_fix-the-thing"
	)
	w := newWorld(t)
	w.pushBranch(t, dep, "stacked.txt", "from-3\n")
	w.gh.AddBlocker(7, fakegh.Issue{Number: 5, State: "open"})
	w.gh.AddBlocker(7, fakegh.Issue{Number: 3, State: "open"})
	w.gh.SeedPull(pull{Head: dep, Base: "main", State: "open"})

	h, _ := writes(map[string]string{"fix.txt": "fixed\n"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if states := w.await(t, "alice", 7); states[len(states)-1] != StatePROpened {
		t.Fatalf("states = %v, want a stacked pull request", states)
	}
	pulls, _ := w.gh.Snapshot()
	got, n := pullByHead(pulls, branch)
	if n != 1 {
		t.Fatalf("pulls = %+v, want one %s", pulls, branch)
	}
	if got.Base != dep {
		t.Errorf("base = %q, want %s", got.Base, dep)
	}
	assertCommitParent(t, w.bare, branch, dep)
}

// seedJobPR pushes the job's branch with one work commit and then a review
// fix commit for each of fixes, and opens its pull request against main.
func (w *world) seedJobPR(t *testing.T, fixes int) {
	t.Helper()
	const branch = "7_fix-the-thing"
	src := t.TempDir()
	git(t, src, "clone", "-q", w.bare, ".")
	git(t, src, "checkout", "-q", "-b", branch)
	for i := range fixes + 1 {
		if err := os.WriteFile(filepath.Join(src, "fix.txt"), []byte(strconv.Itoa(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		msg := "Fix the Thing!"
		if i > 0 {
			msg = fixMessage(7)
		}
		git(t, src, "add", "-A")
		git(t, src, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", msg)
	}
	git(t, src, "push", "-q", "origin", "HEAD:"+branch)
	w.gh.SeedPull(pull{Head: branch, Base: "main"})
}

// TestRedeliveryAfterOutcomeCommentPostsNothing is a job whose outcome was
// posted but whose acknowledgment was lost.
func TestRedeliveryAfterOutcomeCommentPostsNothing(t *testing.T) {
	w := newWorld(t)
	w.seedJobPR(t, 0)
	posted := markOutcome("schmeide opened a pull request for this issue: https://github.test/o/r/pull/1", StatePROpened)
	w.gh.SetComments(7, posted)

	h, sessions := writes(map[string]string{"fix.txt": "x"})
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	if sessions.Load() != 0 {
		t.Errorf("sessions = %d, want none", sessions.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want only the first", pulls)
	}
	if !slices.Equal(comments["7"], []string{posted}) {
		t.Errorf("issue comments = %q, want only the first outcome", comments["7"])
	}
}

// TestRedeliveryKeepsSpentReviewRounds resumes a pull request that already
// had two rejected rounds, so only the last round is left.
func TestRedeliveryKeepsSpentReviewRounds(t *testing.T) {
	w := newWorld(t)
	w.pushConfig(t, testReviewFixConfig)
	w.seedJobPR(t, defaultMaxReviewRounds-1)
	var workers, reviews atomic.Int32
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			reviews.Add(1)
			return writeVerdict(req, "changes\nstill wrong\n")
		}
		workers.Add(1)
		return errors.New("the open pull request needs no new work session")
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed reviewing review_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	if reviews.Load() != 1 || workers.Load() != 0 {
		t.Errorf("workers = %d, reviews = %d, want only the last review round", workers.Load(), reviews.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want only the first", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], fmt.Sprintf("within %d rounds", defaultMaxReviewRounds)) {
		t.Errorf("issue comments = %q, want one comment that the rounds are spent", comments["7"])
	}
	assertNoMerge(t, w)
}
