package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGrok writes a shell script standing in for the grok CLI.
func fakeGrok(t *testing.T, body string) *Grok {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return &Grok{Bin: bin}
}

func TestGrokCheck(t *testing.T) {
	missing := &Grok{Bin: filepath.Join(t.TempDir(), "grok")}
	tests := []struct {
		name    string
		grok    *Grok
		wantErr error
		wantFix string
	}{
		{name: "logged in", grok: fakeGrok(t, "echo 'You are logged in with grok.com.'")},
		{name: "api key", grok: fakeGrok(t, "echo 'You are using XAI_API_KEY.'")},
		{name: "deployment key", grok: fakeGrok(t, "echo 'You are authenticated via deployment key.'")},
		{name: "not installed", grok: missing, wantErr: ErrNotInstalled, wantFix: "install the Grok CLI"},
		{
			name:    "not logged in",
			grok:    fakeGrok(t, "echo 'You are not authenticated.'"),
			wantErr: ErrNotLoggedIn,
			wantFix: "login",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.grok.Check(t.Context())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Check = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && !strings.Contains(err.Error(), tt.wantFix) {
				t.Errorf("Check = %q, want it to mention %q", err, tt.wantFix)
			}
		})
	}

	err := fakeGrok(t, `echo "unknown command" >&2; exit 2`).Check(t.Context())
	if err == nil || errors.Is(err, ErrNotInstalled) || errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("Check with unreadable status = %v, want a distinct error", err)
	}
}

func TestGrokRun(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	grok := fakeGrok(t, `echo "args=$*"; echo "pwd=$(pwd -P)"; echo "prompt=$(cat)"`)
	if err := grok.Run(t.Context(), Request{Dir: dir, Prompt: "--fix the bug", Output: &out}); err != nil {
		t.Fatalf("Run = %v", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "args=-p - --permission-mode acceptEdits\npwd=" + realDir + "\nprompt=--fix the bug\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
	if strings.Contains(strings.Split(out.String(), "\n")[0], "--fix") {
		t.Errorf("prompt was passed as an argument: %q", out.String())
	}

	if err := fakeGrok(t, `echo boom >&2; exit 3`).Run(t.Context(), Request{Dir: dir}); err == nil {
		t.Error("Run with exit 3 = nil, want an error")
	}
	err = grok.Run(t.Context(), Request{Dir: filepath.Join(dir, "nope")})
	if err == nil || errors.Is(err, ErrNotInstalled) {
		t.Errorf("Run in a missing dir = %v, want an error that is not ErrNotInstalled", err)
	}
	missing := &Grok{Bin: filepath.Join(dir, "grok")}
	if err := missing.Run(t.Context(), Request{Dir: dir}); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Run with no binary = %v, want ErrNotInstalled", err)
	}
}

func TestGrokRunArgsReplaceDefault(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	grok := fakeGrok(t, `echo "args=$*"`)
	grok.Args = []string{"--verbose"}
	if err := grok.Run(t.Context(), Request{Dir: dir, Output: &out}); err != nil {
		t.Fatalf("Run = %v", err)
	}
	want := "args=-p - --verbose\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestGrokRunEnvIsWholeEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SCHMEIDE_PARENT_ONLY", "only-parent")
	var out bytes.Buffer
	// ${name+set} is empty when the variable is unset, and "set" when it is present.
	grok := fakeGrok(t, `echo "sentinel=$SCHMEIDE_SENTINEL"; echo "parent=${SCHMEIDE_PARENT_ONLY+set}"`)
	err := grok.Run(t.Context(), Request{
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

func TestGrokCancelKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	// The grandchild inherits stdout, as a tool started by a session would.
	grok := fakeGrok(t, "sleep 300 &\necho $! > child.tmp\nmv child.tmp child.pid\nwait\n")
	ctx, cancel := context.WithCancel(t.Context())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- grok.Run(ctx, Request{Dir: dir, Output: &out}) }()

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

func TestGrokRunLeavesNoChildOnExit(t *testing.T) {
	dir := t.TempDir()
	// The child drops stdout, so the session ends while it is still running.
	grok := fakeGrok(t, "sleep 300 >/dev/null 2>&1 &\necho $! > child.pid\n")
	if err := grok.Run(t.Context(), Request{Dir: dir}); err != nil {
		t.Fatalf("Run = %v", err)
	}
	waitGone(t, childPID(t, dir))
}
