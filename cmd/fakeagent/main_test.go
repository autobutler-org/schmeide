package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRunEditStderrExit(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("FAKEAGENT_TEST", "hi")
	var stderr bytes.Buffer
	code := run("edit out.txt hello world\n\nstderr say $FAKEAGENT_TEST\nexit 3\nstderr unreached\n", &stderr)
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if stderr.String() != "say hi\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
	if b, err := os.ReadFile("out.txt"); err != nil || string(b) != "hello world\n" {
		t.Errorf("out.txt = %q, %v", b, err)
	}
}

func TestRunEmptyScriptExitsZero(t *testing.T) {
	if code := run("", &bytes.Buffer{}); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunBadStepExits2AndNamesIt(t *testing.T) {
	for _, script := range []string{"dance now", "exit nope", "sleep long", "edit onlypath"} {
		var stderr bytes.Buffer
		if code := run(script, &stderr); code != 2 {
			t.Errorf("%q: exit code = %d, want 2", script, code)
		}
		if !strings.Contains(stderr.String(), script) {
			t.Errorf("%q: stderr = %q, want it to name the step", script, stderr.String())
		}
	}
}
