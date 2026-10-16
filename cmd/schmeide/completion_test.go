package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletion(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run([]string{"completion", shell}, &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			for _, w := range []string{"control", "runner", "doctor", "status", "completion"} {
				if !strings.Contains(out.String(), w) {
					t.Errorf("script missing %q", w)
				}
			}
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s not on PATH", shell)
			}
			f := filepath.Join(t.TempDir(), "c")
			if err := os.WriteFile(f, out.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			flag := "-n"
			if shell == "fish" {
				flag = "--no-execute"
			}
			if b, err := exec.Command(bin, flag, f).CombinedOutput(); err != nil {
				t.Fatalf("syntax check: %v\n%s", err, b)
			}
		})
	}
}

func TestCompletionUnknownShell(t *testing.T) {
	for _, args := range [][]string{{"completion", "tcsh"}, {"completion"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code == 0 {
			t.Fatalf("%v: want non-zero exit", args)
		}
		if !strings.Contains(errb.String(), "bash, zsh, fish") {
			t.Errorf("stderr %q does not name the shells", errb.String())
		}
	}
}
