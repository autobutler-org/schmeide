package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/nats-io/nats.go"
)

func TestReportSaysHowToFixEachFailure(t *testing.T) {
	loggedOut := &harness.Fake{CheckErr: fmt.Errorf("%w: run `fake login`", harness.ErrNotLoggedIn)}
	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("nats://127.0.0.1:1", "alice", "", []harness.Harness{loggedOut})) {
		t.Error("Report = true, want false with a logged-out harness and no control node")
	}
	for _, want := range []string{
		"ok    git: ",
		"FAIL  harness fake: harness not logged in: run `fake login`",
		"FAIL  control node: cannot reach the control node's NATS at nats://127.0.0.1:1",
		"Check that `schmeide control` is running",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestReportMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("", "", "", nil)[:1]) || !strings.Contains(out.String(), "FAIL  git: git was not found on PATH; install it") {
		t.Errorf("output = %q, want a failed git check that says to install it", out.String())
	}
}

func TestReportAllGood(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	queue, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: map[string]string{"alice": "alice-nats"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	if err := queue.ServeTokens(func(context.Context, control.Job) (control.Token, error) { return control.Token{}, nil }, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	waitForAliceToken(t, queue.Conn())

	var out bytes.Buffer
	if !Report(t.Context(), &out, Checks("nats://"+addr, "alice", "alice-nats", []harness.Harness{&harness.Fake{}})) {
		t.Errorf("Report = false, want true; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ok    control node: reachable at nats://"+addr) {
		t.Errorf("output %q does not report the control node", out.String())
	}
}

func TestReportEmptySteward(t *testing.T) {
	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("nats://127.0.0.1:1", "", "", nil)) {
		t.Error("Report = true, want false with an empty steward")
	}
	const want = "FAIL  control node: no steward to connect as; pass your GitHub login with --steward"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not contain %q", out.String(), want)
	}
}

func TestReportHarnessNotInstalled(t *testing.T) {
	const fix = "install the fake CLI and put it on PATH"
	missing := &harness.Fake{CheckErr: fmt.Errorf("%w: %s", harness.ErrNotInstalled, fix)}
	var harnessChecks []Check
	for _, c := range Checks("", "", "", []harness.Harness{missing}) {
		if strings.HasPrefix(c.Name, "harness ") {
			harnessChecks = append(harnessChecks, c)
		}
	}
	if len(harnessChecks) != 1 {
		t.Fatalf("harness checks = %d, want 1", len(harnessChecks))
	}
	var out bytes.Buffer
	if Report(t.Context(), &out, harnessChecks) {
		t.Error("Report = true, want false when the harness is not installed")
	}
	want := "FAIL  harness fake: harness not installed: " + fix
	if !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not contain %q", out.String(), want)
	}
	if strings.Contains(out.String(), "not logged in") {
		t.Errorf("output %q reports a login failure; a missing harness is a different failure", out.String())
	}
}

func TestReportWrongRunnerToken(t *testing.T) {
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	queue, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: map[string]string{"alice": "alice-nats"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	if err := queue.ServeTokens(func(context.Context, control.Job) (control.Token, error) { return control.Token{}, nil }, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("nats://"+addr, "alice", "wrong-token", []harness.Harness{&harness.Fake{}})) {
		t.Errorf("Report = true, want false with a wrong runner token; output:\n%s", out.String())
	}
	for _, want := range []string{
		"FAIL  control node:",
		"nats: Authorization Violation",
		"--nats-token is the token its --runner-tokens lists for alice",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

// namedHarness gives a [harness.Harness] its own name. Tests wrap
// [harness.Fake], so Check starts no process and a report can tell two
// fakes apart.
type namedHarness struct {
	harness.Harness
	name string
}

func (n namedHarness) Name() string { return n.name }

// reachableControl starts a control node the same way TestReportAllGood does
// and returns its NATS address. It does not return until alice's token subject
// answers. ServeTokens returns when Subscribe returns, which is before the
// server is serving that subject, so a check that runs immediately can hear
// "no responders" on a busy machine.
func reachableControl(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	queue, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: map[string]string{"alice": "alice-nats"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)
	if err := queue.ServeTokens(func(context.Context, control.Job) (control.Token, error) { return control.Token{}, nil }, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	waitForAliceToken(t, queue.Conn())
	return addr
}

// waitForAliceToken returns once a request to alice's token subject gets a
// reply. Flush lets the server finish the subscription ServeTokens just sent.
// A no-responders reply is retried until the budget runs out, then the test
// fails with what the request returned. Waiting longer would hide a subject
// or permissions bug.
func waitForAliceToken(t *testing.T, nc *nats.Conn) {
	t.Helper()
	const budget = 2 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	if err := nc.FlushWithContext(ctx); err != nil {
		t.Fatalf("flush token subscription: %v", err)
	}
	subject := control.TokenSubjectPrefix + "alice"
	var (
		err   error
		reply []byte
	)
	for {
		var msg *nats.Msg
		msg, err = nc.RequestWithContext(ctx, subject, nil)
		if err == nil {
			return
		}
		if msg != nil {
			reply = msg.Data
		}
		if !errors.Is(err, nats.ErrNoResponders) || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
	}
	t.Fatalf("token request %s returned %v (reply %q)", subject, err, reply)
}

func TestReportPassesWhenOneHarnessIsReady(t *testing.T) {
	addr := reachableControl(t)
	const fix = "install the Grok CLI and put it on PATH"
	missing := namedHarness{
		Harness: &harness.Fake{CheckErr: fmt.Errorf("%w: %s", harness.ErrNotInstalled, fix)},
		name:    "grok",
	}
	ready := namedHarness{Harness: &harness.Fake{}, name: "claude-code"}
	// Both orders. A report that stops at the first error cannot pass just
	// because a later harness is ready.
	for _, hs := range [][]harness.Harness{{missing, ready}, {ready, missing}} {
		var out bytes.Buffer
		if !Report(t.Context(), &out, Checks("nats://"+addr, "alice", "alice-nats", hs)) {
			t.Errorf("Report = false, want true when one harness is ready; output:\n%s", out.String())
		}
		for _, want := range []string{
			"ok    harness claude-code: installed and logged in",
			"skip  harness grok: not selected: harness not installed: " + fix,
			"ok    control node: reachable at nats://" + addr,
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output %q does not contain %q", out.String(), want)
			}
		}
		if strings.Contains(out.String(), "FAIL  harness") {
			t.Errorf("output %q fails a harness the runner will not use", out.String())
		}
	}
}

func TestReportFailsWhenEveryHarnessFails(t *testing.T) {
	addr := reachableControl(t)
	const (
		claudeFix = "install Claude Code and put it on PATH"
		grokFix   = "run `grok login` on this machine, then try again"
	)
	claude := namedHarness{
		Harness: &harness.Fake{CheckErr: fmt.Errorf("%w: %s", harness.ErrNotInstalled, claudeFix)},
		name:    "claude-code",
	}
	grok := namedHarness{
		Harness: &harness.Fake{CheckErr: fmt.Errorf("%w: %s", harness.ErrNotLoggedIn, grokFix)},
		name:    "grok",
	}
	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("nats://"+addr, "alice", "alice-nats", []harness.Harness{claude, grok})) {
		t.Error("Report = true, want false when every harness fails")
	}
	for _, want := range []string{
		"ok    git: ",
		"FAIL  harness claude-code: harness not installed: " + claudeFix,
		"FAIL  harness grok: harness not logged in: " + grokFix,
		"ok    control node: reachable at nats://" + addr,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "not selected") {
		t.Errorf("output %q reports a harness as not selected when none is ready", out.String())
	}
}

func TestReportGitFailsBesideAReadyHarness(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	const fix = "install the Grok CLI and put it on PATH"
	missing := namedHarness{
		Harness: &harness.Fake{CheckErr: fmt.Errorf("%w: %s", harness.ErrNotInstalled, fix)},
		name:    "grok",
	}
	ready := namedHarness{Harness: &harness.Fake{}, name: "claude-code"}
	var checks []Check
	for _, c := range Checks("", "alice", "", []harness.Harness{missing, ready}) {
		if c.Name != "control node" {
			checks = append(checks, c)
		}
	}
	var out bytes.Buffer
	if Report(t.Context(), &out, checks) {
		t.Errorf("Report = true, want false when git is missing; output:\n%s", out.String())
	}
	for _, want := range []string{
		"FAIL  git: git was not found on PATH; install it",
		"skip  harness grok: not selected: harness not installed: " + fix,
		"ok    harness claude-code: installed and logged in",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q does not contain %q", out.String(), want)
		}
	}
}

func TestReportControlNodeFailsBesideAReadyHarness(t *testing.T) {
	const fix = "install the Grok CLI and put it on PATH"
	missing := namedHarness{
		Harness: &harness.Fake{CheckErr: fmt.Errorf("%w: %s", harness.ErrNotInstalled, fix)},
		name:    "grok",
	}
	ready := namedHarness{Harness: &harness.Fake{}, name: "claude-code"}
	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("nats://127.0.0.1:1", "", "", []harness.Harness{missing, ready})) {
		t.Errorf("Report = true, want false when the control node has no steward; output:\n%s", out.String())
	}
	const want = "FAIL  control node: no steward to connect as; pass your GitHub login with --steward"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output %q does not contain %q", out.String(), want)
	}
}
