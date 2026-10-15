package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeClaude writes a shell script standing in for the claude CLI.
func fakeClaude(t *testing.T, body string) *ClaudeCode {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return &ClaudeCode{Bin: bin}
}

func TestFakeThroughInterface(t *testing.T) {
	fake := &Fake{RunFunc: func(ctx context.Context, req Request) error {
		_, _ = req.Output.Write([]byte("working in " + req.Dir))
		<-ctx.Done()
		return ctx.Err()
	}}
	var h Harness = fake

	if err := h.Check(t.Context()); err != nil {
		t.Fatalf("Check = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, Request{Dir: "/work", Prompt: "do it", Output: &out}) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run after cancel = %v, want context.Canceled", err)
	}
	if out.String() != "working in /work" {
		t.Errorf("output = %q", out.String())
	}
	if len(fake.Requests) != 1 || fake.Requests[0].Prompt != "do it" {
		t.Errorf("Requests = %+v", fake.Requests)
	}
	if err := (&Fake{}).Run(t.Context(), Request{}); err != nil {
		t.Errorf("default Run = %v, want nil", err)
	}
}

func TestAvailable(t *testing.T) {
	ready := &Fake{}
	got := Available(t.Context(), []Harness{&Fake{CheckErr: ErrNotLoggedIn}, ready})
	if len(got) != 1 || got[0] != Harness(ready) {
		t.Errorf("Available = %v, want only the ready harness", got)
	}
}

// firstPassing is the runner's rule: the first harness whose Check passes.
func firstPassing(ctx context.Context, hs []Harness) Harness {
	for _, h := range hs {
		if h.Check(ctx) == nil {
			return h
		}
	}
	return nil
}

func TestAllOrdersClaudeCodeBeforeGrok(t *testing.T) {
	all := All()
	if len(all) != 2 || all[0].Name() != "claude-code" || all[1].Name() != "grok" {
		names := make([]string, len(all))
		for i, h := range all {
			names[i] = h.Name()
		}
		t.Fatalf("All() = %q, want claude-code then grok", names)
	}
}

func TestGrokSelectedOnlyWhenClaudeCodeIsNotUsable(t *testing.T) {
	claude := fakeClaude(t, `echo '{"loggedIn": true}'`)
	grok := fakeGrok(t, `echo 'You are logged in with grok.com.'`)
	// The pair is All()'s order: Claude Code, then Grok.
	all := All()
	pair := []Harness{claude, grok}
	if len(all) != len(pair) {
		t.Fatalf("All() has %d harnesses, pair has %d", len(all), len(pair))
	}
	for i := range all {
		if all[i].Name() != pair[i].Name() {
			t.Fatalf("All()[%d] = %s, pair has %s", i, all[i].Name(), pair[i].Name())
		}
	}

	if got := firstPassing(t.Context(), pair); got == nil || got.Name() != "claude-code" {
		t.Fatalf("both usable: chose %v, want claude-code", got)
	}

	missing := &ClaudeCode{Bin: filepath.Join(t.TempDir(), "claude")}
	if got := firstPassing(t.Context(), []Harness{missing, grok}); got == nil || got.Name() != "grok" {
		t.Fatalf("claude missing: chose %v, want grok", got)
	}
	loggedOut := fakeClaude(t, `echo '{"loggedIn": false}'; exit 1`)
	if got := firstPassing(t.Context(), []Harness{loggedOut, grok}); got == nil || got.Name() != "grok" {
		t.Fatalf("claude logged out: chose %v, want grok", got)
	}
	grokOut := fakeGrok(t, `echo 'You are not authenticated.'`)
	if got := firstPassing(t.Context(), []Harness{missing, grokOut}); got != nil {
		t.Fatalf("neither usable: chose %s, want none", got.Name())
	}
}

func TestClaudeCheck(t *testing.T) {
	missing := &ClaudeCode{Bin: filepath.Join(t.TempDir(), "claude")}
	tests := []struct {
		name    string
		claude  *ClaudeCode
		wantErr error
		wantFix string
	}{
		{name: "logged in", claude: fakeClaude(t, `echo '{"loggedIn": true}'`)},
		{name: "not installed", claude: missing, wantErr: ErrNotInstalled, wantFix: "install Claude Code"},
		{
			name:    "not logged in",
			claude:  fakeClaude(t, `echo '{"loggedIn": false}'; exit 1`),
			wantErr: ErrNotLoggedIn, wantFix: "auth login",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.claude.Check(t.Context())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Check = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && !strings.Contains(err.Error(), tt.wantFix) {
				t.Errorf("Check = %q, want it to mention %q", err, tt.wantFix)
			}
		})
	}

	err := fakeClaude(t, `echo "unknown command" >&2; exit 2`).Check(t.Context())
	if err == nil || errors.Is(err, ErrNotInstalled) || errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("Check with unreadable status = %v, want a distinct error", err)
	}
}

func TestClaudeRun(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	claude := fakeClaude(t, `echo "args=$*"; echo "pwd=$(pwd -P)"; echo "prompt=$(cat)"`)
	if err := claude.Run(t.Context(), Request{Dir: dir, Prompt: "--fix the bug", Output: &out}); err != nil {
		t.Fatalf("Run = %v", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "args=--print --permission-mode acceptEdits\npwd=" + realDir + "\nprompt=--fix the bug\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}

	if err := fakeClaude(t, `echo boom >&2; exit 3`).Run(t.Context(), Request{Dir: dir}); err == nil {
		t.Error("Run with exit 3 = nil, want an error")
	}
	err = claude.Run(t.Context(), Request{Dir: filepath.Join(dir, "nope")})
	if err == nil || errors.Is(err, ErrNotInstalled) {
		t.Errorf("Run in a missing dir = %v, want an error that is not ErrNotInstalled", err)
	}
	missing := &ClaudeCode{Bin: filepath.Join(dir, "claude")}
	if err := missing.Run(t.Context(), Request{Dir: dir}); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Run with no binary = %v, want ErrNotInstalled", err)
	}
}

func TestClaudeRunArgsReplaceDefault(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	claude := fakeClaude(t, `echo "args=$*"`)
	claude.Args = []string{"--verbose"}
	if err := claude.Run(t.Context(), Request{Dir: dir, Output: &out}); err != nil {
		t.Fatalf("Run = %v", err)
	}
	want := "args=--print --verbose\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestClaudeRunEnvIsWholeEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCHMEIDE_PARENT_ONLY", "only-parent")
	var out bytes.Buffer
	// ${name+set} is empty when the variable is unset, and "set" when it is present.
	claude := fakeClaude(t, `echo "sentinel=$SCHMEIDE_SENTINEL"; echo "parent=${SCHMEIDE_PARENT_ONLY+set}"`)
	err := claude.Run(t.Context(), Request{
		Dir:    dir,
		Env:    []string{"SCHMEIDE_SENTINEL=present"},
		Output: &out,
	})
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	want := "sentinel=present\nparent=\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// waitGone fails the test unless the process disappears.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

// childPID waits for the fake CLI to record the PID of its child.
func childPID(t *testing.T, dir string) int {
	t.Helper()
	for range 100 {
		if b, err := os.ReadFile(filepath.Join(dir, "child.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("fake CLI never wrote child.pid")
	return 0
}

func TestClaudeCancelKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	// The grandchild inherits stdout, as a tool started by a session would.
	claude := fakeClaude(t, "sleep 300 &\necho $! > child.tmp\nmv child.tmp child.pid\nwait\n")
	ctx, cancel := context.WithCancel(t.Context())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- claude.Run(ctx, Request{Dir: dir, Output: &out}) }()

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
}

func TestClaudeRunLeavesNoChildOnExit(t *testing.T) {
	dir := t.TempDir()
	// The child drops stdout, so the session ends while it is still running.
	claude := fakeClaude(t, "sleep 300 >/dev/null 2>&1 &\necho $! > child.pid\n")
	if err := claude.Run(t.Context(), Request{Dir: dir}); err != nil {
		t.Fatalf("Run = %v", err)
	}
	waitGone(t, childPID(t, dir))
}
