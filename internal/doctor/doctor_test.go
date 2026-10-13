package doctor

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
)

func TestReportSaysHowToFixEachFailure(t *testing.T) {
	loggedOut := &harness.Fake{CheckErr: fmt.Errorf("%w: run `fake login`", harness.ErrNotLoggedIn)}
	var out bytes.Buffer
	if Report(t.Context(), &out, Checks("nats://127.0.0.1:1", "", []harness.Harness{loggedOut})) {
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
	if Report(t.Context(), &out, Checks("", "", nil)[:1]) || !strings.Contains(out.String(), "FAIL  git: git was not found on PATH; install it") {
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
	queue, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(queue.Close)

	var out bytes.Buffer
	if !Report(t.Context(), &out, Checks("nats://"+addr, "", []harness.Harness{&harness.Fake{}})) {
		t.Errorf("Report = false, want true; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "ok    control node: reachable at nats://"+addr) {
		t.Errorf("output %q does not report the control node", out.String())
	}
}
