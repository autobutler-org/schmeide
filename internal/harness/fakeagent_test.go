package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeagentBin is cmd/fakeagent, built by TestMain.
var fakeagentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeagent")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeagentBin = filepath.Join(dir, "fakeagent")
	out, err := exec.Command("go", "build", "-o", fakeagentBin, "github.com/autobutler-org/schmeide/cmd/fakeagent").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build fakeagent: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeagentHarnesses returns the real harnesses pointed at fakeagent.
func fakeagentHarnesses() []Harness {
	return []Harness{&ClaudeCode{Bin: fakeagentBin}, &Grok{Bin: fakeagentBin}}
}

func scriptEnv(script string) []string {
	return []string{"FAKEAGENT_SCRIPT=" + script}
}

func TestFakeagentEditAndExit0(t *testing.T) {
	for _, h := range fakeagentHarnesses() {
		t.Run(h.Name(), func(t *testing.T) {
			dir := t.TempDir()
			var out bytes.Buffer
			err := h.Run(t.Context(), Request{Dir: dir, Env: scriptEnv("edit done.txt finished\nexit 0"), Output: &out})
			if err != nil {
				t.Fatalf("Run = %v; output %q", err, out.String())
			}
			if b, err := os.ReadFile(filepath.Join(dir, "done.txt")); err != nil || string(b) != "finished\n" {
				t.Errorf("done.txt = %q, %v", b, err)
			}
		})
	}
}

func TestFakeagentExit1Fails(t *testing.T) {
	for _, h := range fakeagentHarnesses() {
		t.Run(h.Name(), func(t *testing.T) {
			if err := h.Run(t.Context(), Request{Dir: t.TempDir(), Env: scriptEnv("exit 1")}); err == nil {
				t.Error("Run = nil, want an error")
			}
		})
	}
}

func TestFakeagentHangCancelKillsGroup(t *testing.T) {
	for _, h := range fakeagentHarnesses() {
		t.Run(h.Name(), func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- h.Run(ctx, Request{Dir: dir, Env: scriptEnv("edit child.pid $$\nhang")})
			}()

			pid := childPID(t, dir)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Run after cancel = %v, want context.Canceled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after cancel")
			}
			waitGone(t, pid)
		})
	}
}

func TestFakeagentReceivesWholeEnv(t *testing.T) {
	for _, h := range fakeagentHarnesses() {
		t.Run(h.Name(), func(t *testing.T) {
			var out bytes.Buffer
			env := append(scriptEnv("stderr xai=$XAI_API_KEY secret=$SCHMEIDE_SECRET"),
				"XAI_API_KEY=xai-value", "SCHMEIDE_SECRET=secret-value")
			if err := h.Run(t.Context(), Request{Dir: t.TempDir(), Env: env, Output: &out}); err != nil {
				t.Fatalf("Run = %v; output %q", err, out.String())
			}
			if got, want := strings.TrimSpace(out.String()), "xai=xai-value secret=secret-value"; got != want {
				t.Errorf("output = %q, want %q", got, want)
			}
		})
	}
}

// A session in the runner's process session would be a background group on
// the runner's terminal and stop on SIGTTOU, so it must lead its own session.
func TestFakeagentRunsInItsOwnSession(t *testing.T) {
	for _, h := range fakeagentHarnesses() {
		t.Run(h.Name(), func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- h.Run(ctx, Request{Dir: dir, Env: scriptEnv("edit child.pid $$\nhang")})
			}()

			pid := childPID(t, dir)
			out, err := exec.Command("ps", "-o", "sid=", "-p", strconv.Itoa(pid)).Output()
			cancel()
			<-done
			if err != nil {
				t.Fatalf("ps for pid %d = %v", pid, err)
			}
			if sid := strings.TrimSpace(string(out)); sid != strconv.Itoa(pid) {
				t.Errorf("session id = %s, want %d: the session shares the runner's session and terminal", sid, pid)
			}
		})
	}
}
