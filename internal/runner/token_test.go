package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
)

// staticToken is a worktree's token source that never changes.
func staticToken(token string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return token, nil }
}

func TestTokenStale(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		token control.Token
		want  bool
	}{
		{"none yet", control.Token{}, true},
		{"no expiry is never stale", control.Token{Token: "t"}, false},
		{"well before the margin", control.Token{Token: "t", ExpiresAt: now.Add(time.Hour)}, false},
		{"a nanosecond before the margin", control.Token{Token: "t", ExpiresAt: now.Add(tokenRefreshMargin + time.Nanosecond)}, false},
		{"at the margin", control.Token{Token: "t", ExpiresAt: now.Add(tokenRefreshMargin)}, true},
		{"inside the margin", control.Token{Token: "t", ExpiresAt: now.Add(time.Minute)}, true},
		{"expired", control.Token{Token: "t", ExpiresAt: now.Add(-time.Minute)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stale(tc.token, now); got != tc.want {
				t.Errorf("stale(%+v) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// TestJobTokenRefreshesOnlyWhenStale asks the control node once for a token
// with an hour left, and again for each one inside the refresh margin.
func TestJobTokenRefreshesOnlyWhenStale(t *testing.T) {
	var fetches atomic.Int32
	life := time.Hour
	tokens := &jobToken{fetch: func(context.Context) (control.Token, error) {
		n := fetches.Add(1)
		return control.Token{Token: fmt.Sprintf("t%d", n), ExpiresAt: time.Now().Add(life)}, nil
	}}
	for range 3 {
		if got, err := tokens.get(t.Context()); err != nil || got != "t1" {
			t.Fatalf("get = %q, %v, want t1", got, err)
		}
	}
	life = time.Minute
	tokens.cur.ExpiresAt = time.Now()
	if got, err := tokens.get(t.Context()); err != nil || got != "t2" {
		t.Fatalf("get after expiry = %q, %v, want t2", got, err)
	}
	if got, err := tokens.get(t.Context()); err != nil || got != "t3" {
		t.Fatalf("get with a minute left = %q, %v, want t3", got, err)
	}
	if got := tokens.current(); got != "t3" {
		t.Errorf("current = %q, want t3, the latest token", got)
	}
	if got := (&jobToken{}).current(); got != "" {
		t.Errorf("current before any get = %q, want empty", got)
	}

	down := errors.New("no control node")
	failing := &jobToken{fetch: func(context.Context) (control.Token, error) { return control.Token{}, down }}
	if _, err := failing.get(t.Context()); !errors.Is(err, down) || !strings.Contains(err.Error(), "refresh the job's GitHub token") {
		t.Errorf("get with a failing control node = %v, want it to say the refresh failed", err)
	}
}

// expiringTokens makes the job's first token expire life from now, and
// every later one an hour from now. The control node reports each expiry
// tokenRefreshMargin later than the fake GitHub enforces it, so GitHub
// refuses the first token at the moment the runner should stop using it.
func (w *world) expiringTokens(life time.Duration) *atomic.Int32 {
	var mints atomic.Int32
	w.mint = func(context.Context, control.Job) (control.Token, error) {
		n := mints.Add(1)
		lasts := time.Hour
		if n == 1 {
			lasts = life
		}
		token := fmt.Sprintf("job-token-%d", n)
		expires := time.Now().Add(lasts)
		w.gh.Accept(token, expires)
		return control.Token{Token: token, ExpiresAt: expires.Add(tokenRefreshMargin)}, nil
	}
	return &mints
}

func assertNoRefusedCalls(t *testing.T, w *world) {
	t.Helper()
	for _, call := range w.gh.Calls() {
		if strings.HasPrefix(call, "401 ") {
			t.Errorf("GitHub refused an expired token: %s", call)
		}
	}
}

// TestJobOutlivingItsTokenOpensPR runs a session that outlasts the job's
// first token. The pull request, the push before it, and the comment after
// it use a fresh token.
func TestJobOutlivingItsTokenOpensPR(t *testing.T) {
	w := newWorld(t)
	mints := w.expiringTokens(300 * time.Millisecond)
	h := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			return writeVerdict(req, "pass\n")
		}
		time.Sleep(500 * time.Millisecond)
		return os.WriteFile(filepath.Join(req.Dir, "fix.txt"), []byte("fixed\n"), 0o600)
	}}
	w.start(t, "alice", h)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working reviewing pr_opened]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want one", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "opened a pull request") {
		t.Errorf("issue comments = %q, want the pull request comment", comments["7"])
	}
	if n := mints.Load(); n != 2 {
		t.Errorf("tokens minted = %d, want 2: one at claim, one after the session", n)
	}
	// The revoke uses the second token: the expired first one would be a 401.
	w.awaitRevoked(t, 1)
	assertNoRefusedCalls(t, w)
}

// TestSessionTimeoutAfterTokenExpiryStillComments is the production case:
// the session times out after the job's first token expired, and the
// failure comment still reaches the issue.
func TestSessionTimeoutAfterTokenExpiryStillComments(t *testing.T) {
	w := newWorld(t)
	w.maxAttempts = 1
	w.expiringTokens(100 * time.Millisecond)
	h := &harness.Fake{RunFunc: func(ctx context.Context, _ harness.Request) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	r, _ := w.runner(t, "alice", h)
	r.opts.SessionTimeout = 300 * time.Millisecond
	w.run(t, r)
	w.publish(t, "alice", 7)

	if got, want := fmt.Sprint(w.await(t, "alice", 7)), "[claimed working failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	_, comments := w.gh.Snapshot()
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "session timed out") {
		t.Errorf("issue comments = %q, want one saying the session timed out", comments["7"])
	}
	assertNoRefusedCalls(t, w)
}

// TestWorktreeGitUsesTheCurrentToken reads git's http.extraHeader the way a
// push sees it, before and after the token changes.
func TestWorktreeGitUsesTheCurrentToken(t *testing.T) {
	w := newWorld(t)
	var current atomic.Value
	current.Store("first")
	ws, err := newWorktree(t.Context(), worktreeSpec{
		workDir: t.TempDir(), url: w.bare, base: "main", branch: "7_x",
		token:   func(context.Context) (string, error) { return current.Load().(string), nil },
		gitName: "n", gitEmail: "e@example.test", id: "1-7", repoID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	header := func(token string) string {
		return "Authorization: Basic " + basicAuth(token)
	}
	for _, token := range []string{"first", "second"} {
		current.Store(token)
		got, err := ws.git(t.Context(), ws.dir, "config", "--get", "http.extraHeader")
		if err != nil || got != header(token) {
			t.Errorf("http.extraHeader = %q, %v, want the header for %q", got, err, token)
		}
	}
}
