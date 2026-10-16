package secretscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanReportsEachCredentialShape(t *testing.T) {
	t.Parallel()
	github := "gh" + "p_" + strings.Repeat("a", 36)
	githubPat := "github_" + "pat_" + strings.Repeat("b", 22) + "_" + strings.Repeat("c", 59)
	gitlab := "gl" + "pat-" + strings.Repeat("d", 20)
	aws := "AKI" + "AIOSFODNN7EXAMPLE"
	jwt := "ey" + "JhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	tests := []struct {
		name    string
		literal string
		kind    string
	}{
		{name: "PKCS8 private key", literal: "-----BEGIN " + "PRIVATE KEY-----", kind: KindPEM},
		{name: "RSA private key", literal: "-----BEGIN " + "RSA PRIVATE KEY-----", kind: KindPEM},
		{name: "OpenSSH private key", literal: "-----BEGIN " + "OPENSSH PRIVATE KEY-----", kind: KindPEM},
		{name: "GitHub token", literal: github, kind: KindGitHub},
		{name: "GitHub fine-grained token", literal: githubPat, kind: KindGitHub},
		{name: "GitLab token", literal: gitlab, kind: KindGitLab},
		{name: "AWS access key", literal: aws, kind: KindAWS},
		{name: "JWT", literal: jwt, kind: KindJWT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := scan("secrets/leak.txt", []byte("value = \""+tt.literal+"\"\n"))
			if len(got) != 1 {
				t.Fatalf("scan findings = %d, want 1", len(got))
			}
			if got[0].File != "secrets/leak.txt" || got[0].Line != 1 || got[0].Kind != tt.kind {
				t.Fatalf("finding = %+v, want file secrets/leak.txt line 1 kind %s", got[0], tt.kind)
			}
			if strings.Contains(got[0].File, tt.literal) || strings.Contains(got[0].Kind, tt.literal) {
				t.Fatal("finding includes the secret text")
			}
		})
	}
}

func TestScanSkipsAllowCommentOnTheSameLine(t *testing.T) {
	t.Parallel()
	secret := "gh" + "p_" + strings.Repeat("a", 36)
	text := "token = \"" + secret + "\" // schmeide:allow-secret\n" +
		"other = \"" + secret + "\"\n"
	got := scan("cfg.go", []byte(text))
	if len(got) != 1 || got[0].Line != 2 || got[0].Kind != KindGitHub || got[0].File != "cfg.go" {
		t.Fatalf("scan = %+v, want only the unmarked line", got)
	}
	if strings.Contains(got[0].Kind, secret) {
		t.Fatal("finding includes the secret text")
	}
}

func TestScanAllowsACleanFile(t *testing.T) {
	t.Parallel()
	src := "package p\n\n" +
		"const placeholder = \"ghp_too_short\"\n" +
		"const region = \"AKIA\"\n" +
		"const gl = \"glpat-short\"\n" +
		"const dotted = \"not.a.jwt\"\n" +
		"const pub = \"-----BEGIN " + "PUBLIC KEY-----\"\n"
	if got := scan("clean.go", []byte(src)); len(got) != 0 {
		t.Fatalf("clean file findings = %+v", got)
	}
}

func TestScanReadsTrackedWorktreeFilesOnly(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "dev@example.com")
	git("config", "user.name", "Dev")
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "ok.txt")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")

	secret := "gh" + "p_" + strings.Repeat("e", 36)
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte(secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", secret)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	got, err := Scan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("untracked file and environment findings = %+v", got)
	}

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("see "+secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "track a secret")

	got, err = Scan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "tracked.txt" || got[0].Line != 1 || got[0].Kind != KindGitHub {
		t.Fatalf("scan = %+v, want tracked.txt line 1", got)
	}
	if strings.Contains(got[0].Kind, secret) || strings.Contains(got[0].File, secret) {
		t.Fatal("finding includes the secret text")
	}
}

func TestScanSkipsAGitWrapperOnPath(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "dev@example.com")
	git("config", "user.name", "Dev")
	secret := "gl" + "pat-" + strings.Repeat("f", 20)
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte(secret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "track")

	wrapper := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(wrapper, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := Scan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "tracked.txt" || got[0].Kind != KindGitLab {
		t.Fatalf("scan = %+v, want the tracked GitLab token", got)
	}
}

func TestScanRequiresAGitWorktree(t *testing.T) {
	if _, err := Scan(t.Context(), t.TempDir()); err == nil {
		t.Fatal("Scan succeeded on a directory that is not a git worktree")
	}
}
