package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/repoconfig"
)

func TestInitWritesBothFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, errb bytes.Buffer
	if code := run([]string{"init"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	cfg, err := repoconfig.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gate != "make check test" {
		t.Errorf("gate = %q", cfg.Gate)
	}
	if b, err := os.ReadFile(initControlPath); err != nil || !strings.Contains(string(b), "replace-me") {
		t.Errorf("control.conf missing or without placeholders: %v", err)
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	for _, keep := range []string{repoconfig.Path, initControlPath} {
		t.Run(keep, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var out, errb bytes.Buffer
			if run([]string{"init"}, &out, &errb) != 0 {
				t.Fatal(errb.String())
			}
			// Edit both so unchanged bytes prove nothing was rewritten.
			before := map[string][]byte{}
			for _, p := range []string{repoconfig.Path, initControlPath} {
				b := []byte("edited " + p + "\n")
				if err := os.WriteFile(p, b, 0o600); err != nil {
					t.Fatal(err)
				}
				before[p] = b
			}
			if err := os.Remove(map[string]string{repoconfig.Path: initControlPath, initControlPath: repoconfig.Path}[keep]); err != nil {
				t.Fatal(err)
			}
			delete(before, map[string]string{repoconfig.Path: initControlPath, initControlPath: repoconfig.Path}[keep])
			errb.Reset()
			if code := run([]string{"init"}, &out, &errb); code == 0 {
				t.Fatal("second init succeeded")
			}
			if !strings.Contains(errb.String(), keep) {
				t.Errorf("error does not name %s: %s", keep, errb.String())
			}
			for p, want := range before {
				if got, _ := os.ReadFile(p); !bytes.Equal(got, want) {
					t.Errorf("%s changed", p)
				}
			}
			other := map[string]string{repoconfig.Path: initControlPath, initControlPath: repoconfig.Path}[keep]
			if _, err := os.Stat(other); err == nil {
				t.Errorf("%s was created despite refusal", other)
			}
		})
	}
}
