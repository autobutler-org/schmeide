package repomap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMapNamesTrackedGoSymbols(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := newRepo(t)

	const src = "package p\n\nfunc Hello() {}\n\nfunc (w Widget) Greet() {}\n\ntype Widget struct{}\n\ntype Alias = Widget\n"
	writeFile(t, filepath.Join(dir, "widget.go"), src)
	writeFile(t, filepath.Join(dir, "notes.txt"), "func NotAFunction\n")
	git(t, dir, "add", "widget.go", "notes.txt")
	git(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "add files")
	writeFile(t, filepath.Join(dir, "untracked.go"), "package p\n\nfunc HiddenSecret() {}\n")

	got, err := Map(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"widget.go", "Hello", "Greet", "Widget", "Alias", "notes.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("map = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "untracked.go") || strings.Contains(got, "HiddenSecret") || strings.Contains(got, "NotAFunction") {
		t.Fatalf("map = %q, want tracked names only", got)
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("map = %q, want the whole map", got)
	}
	if len(got) > maxBytes {
		t.Fatalf("len = %d, want at most %d", len(got), maxBytes)
	}
}

func TestMapTruncatesPastEightKiB(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := newRepo(t)

	var b strings.Builder
	b.WriteString("package p\n")
	const n = 600
	for i := range n {
		fmt.Fprintf(&b, "func Function%04d() {}\n", i)
	}
	writeFile(t, filepath.Join(dir, "big.go"), b.String())
	git(t, dir, "add", "big.go")
	git(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "add a large file")

	got, err := Map(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxBytes {
		t.Fatalf("len = %d, want at most %d", len(got), maxBytes)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("map = %q, want it to say it was truncated", got)
	}
	if !strings.Contains(got, "big.go") || !strings.Contains(got, "Function0000") {
		t.Fatalf("map = %q, want the start of the file", got)
	}
	if strings.Contains(got, "Function0599") {
		t.Fatal("map kept a function past the cap")
	}
}

func TestMapEmptyWhenNotACheckout(t *testing.T) {
	got, err := Map(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("map = %q, want empty", got)
	}
}

func TestMapEmptyCheckout(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	got, err := Map(newRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("map = %q, want empty", got)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "dev@example.com")
	git(t, dir, "config", "user.name", "Dev")
	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
