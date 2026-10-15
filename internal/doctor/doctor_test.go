package doctor

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
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
