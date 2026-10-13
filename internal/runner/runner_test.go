package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/nats-io/nats.go"
)

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

type pull struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
	URL   string `json:"html_url"`
}

// fakeGitHub is the part of the GitHub REST API a runner uses, for testRepo.
type fakeGitHub struct {
	t *testing.T
	// onPR, when set, is called after a pull request is created.
	onPR func()

	mu       sync.Mutex
	pulls    []pull
	comments map[string][]string // by issue number
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("%s %s: Authorization = %q, want the job's token", r.Method, r.URL, got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/repos/"+testRepo)
	reply := any(nil)
	switch {
	case r.Method == http.MethodGet && path == "":
		reply = map[string]string{"default_branch": "main"}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/issues/"):
		reply = map[string]string{"title": "Fix the Thing!", "body": "The thing is broken."}
	case r.Method == http.MethodGet && path == "/pulls":
		found := []pull{}
		for _, p := range f.pulls {
			if "o:"+p.Head == r.URL.Query().Get("head") {
				found = append(found, p)
			}
		}
		reply = found
	case r.Method == http.MethodPost && path == "/pulls":
		var p pull
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			f.t.Error(err)
		}
		p.URL = fmt.Sprintf("https://github.test/o/r/pull/%d", len(f.pulls)+1)
		f.pulls = append(f.pulls, p)
		reply = p
		if f.onPR != nil {
			f.onPR()
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/comments"):
		var c struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			f.t.Error(err)
		}
		issue := strings.Split(path, "/")[2]
		f.comments[issue] = append(f.comments[issue], c.Body)
		reply = c
	default:
		f.t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL)
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeGitHub) snapshot() ([]pull, map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	comments := map[string][]string{}
	for k, v := range f.comments {
		comments[k] = append([]string(nil), v...)
	}
	return append([]pull(nil), f.pulls...), comments
}

type statusMsg struct {
	subject string
	Status
}

// world is a control node queue, a fake GitHub, and a git remote holding
// testRepo, whose gate fails when a file named "broken" exists.
type world struct {
	queue    *control.Queue
	natsURL  string
	gh       *fakeGitHub
	apiURL   string
	gitRoot  string
	bare     string
	client   *nats.Conn
	statuses chan statusMsg
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
	queue, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	log := slog.New(slog.DiscardHandler)
	if err := queue.ServeTokens(func(context.Context, control.Job) (control.Token, error) {
		return control.Token{Token: testToken}, nil
	}, log); err != nil {
		t.Fatal(err)
	}

	w := &world{
		queue:    queue,
		natsURL:  "nats://" + addr,
		gh:       &fakeGitHub{t: t, comments: map[string][]string{}},
		gitRoot:  t.TempDir(),
		statuses: make(chan statusMsg, 64),
	}
	api := httptest.NewServer(w.gh)
	t.Cleanup(api.Close)
	w.apiURL = api.URL

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

	if w.client, err = nats.Connect(w.natsURL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.client.Close)
	if _, err := w.client.Subscribe(StatusSubjectPrefix+">", func(m *nats.Msg) {
		s := statusMsg{subject: m.Subject}
		if err := json.Unmarshal(m.Data, &s.Status); err != nil {
			t.Error(err)
		}
		w.statuses <- s
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) publish(t *testing.T, steward string, issue int) {
	t.Helper()
	job := control.Job{Repo: testRepo, RepoID: testRepoID, Issue: issue, Steward: steward, InstallationID: 1}
	if created, err := w.queue.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("publish job: created=%v err=%v", created, err)
	}
}

// start runs a runner for the steward until the test ends and returns it
// along with its work directory.
func (w *world) start(t *testing.T, steward string, h harness.Harness) (*Runner, string) {
	t.Helper()
	workDir := t.TempDir()
	r, err := New(t.Context(), Options{
		NATSURL:      w.natsURL,
		Steward:      steward,
		WorkDir:      workDir,
		GitHubAPIURL: w.apiURL,
		GitHubURL:    w.gitRoot,
		GitName:      "schmeide[bot]",
		GitEmail:     "schmeide[bot]@example.test",
		Harnesses:    []harness.Harness{h},
		AckWait:      time.Second,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
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
	return r, workDir
}

// await returns the states reported for the issue, up to and including the
// first final one.
func (w *world) await(t *testing.T, steward string, issue int) []State {
	t.Helper()
	subject := fmt.Sprintf("%s%s.%d.%d", StatusSubjectPrefix, steward, testRepoID, issue)
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
			if s.State == StateGateFailed || s.State == StatePROpened || s.State == StateFailed {
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

	var prompt atomic.Value
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		prompt.Store(req.Prompt)
		if err := os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("fixed\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(req.Dir, ".schmeide", "pr.md"), []byte("7: fix the thing\n\nIt was broken.\n"), 0o600)
	}}
	_, workDir := w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	select {
	case reg := <-registered:
		if reg.Steward != "alice" || fmt.Sprint(reg.Harnesses) != "[fake]" {
			t.Errorf("registration = %+v, want steward alice with harness fake", reg)
		}
	case <-time.After(5 * time.Second):
		t.Error("runner did not register")
	}
	if p, _ := prompt.Load().(string); !strings.Contains(p, "Fix the Thing!") || !strings.Contains(p, "The thing is broken.") || !strings.Contains(p, "#7") {
		t.Errorf("brief does not carry the issue: %q", p)
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

	pulls, comments := w.gh.snapshot()
	if len(pulls) != 1 {
		t.Fatalf("pull requests = %+v, want exactly one", pulls)
	}
	want := pull{Title: "7: fix the thing", Body: "It was broken.\n\nCloses #7", Head: branch, Base: "main", URL: pulls[0].URL}
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

func TestGateFailureOpensNoPR(t *testing.T) {
	w := newWorld(t)
	h, _ := writes(map[string]string{"broken": "x"})
	_, workDir := w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working gate_failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	pulls, comments := w.gh.snapshot()
	if len(pulls) != 0 {
		t.Errorf("pull requests = %+v, want none", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "gate command `test ! -e broken` failed") {
		t.Errorf("issue comments = %q, want one reporting the gate failure", comments["7"])
	}
	if branches := git(t, w.bare, "branch", "--list", "7_*"); branches != "" {
		t.Errorf("branch pushed despite the gate failure: %s", branches)
	}
	if left, _ := os.ReadDir(filepath.Join(workDir, "worktrees")); len(left) != 0 {
		t.Errorf("worktrees left behind: %v", left)
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
	if got := sessions.Load(); got != 1 {
		t.Errorf("sessions = %d, want 1: only alice's job", got)
	}
	pulls, comments := w.gh.snapshot()
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
		{name: "dies mid-session", wantSessions: 1},
		{name: "dies after opening the PR", afterPR: true, wantSessions: 0},
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
				w.gh.onPR = die
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
			if pulls, _ := w.gh.snapshot(); len(pulls) != 1 {
				t.Errorf("pull requests = %+v, want exactly one", pulls)
			}
			if got := git(t, w.bare, "show", "7_fix-the-thing:fix.txt"); got != "x" {
				t.Errorf("pushed fix.txt = %q, want %q", got, "x")
			}
		})
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
	_, err := New(t.Context(), Options{NATSURL: w.natsURL, Steward: "alice", Harnesses: []harness.Harness{h}},
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
