package secretscan

import (
	"bytes"
	"fmt"
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
	ghToken := func(prefix string) string { return "gh" + prefix + "_" + strings.Repeat("A1", 18) }
	anthropic := "sk-" + "ant-api03-" + strings.Repeat("Zx_9-", 8)
	xai := "xa" + "i-" + strings.Repeat("Q7", 20)
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
		{name: "GitHub installation token", literal: ghToken("s"), kind: KindGitHub},
		{name: "GitHub OAuth token", literal: ghToken("o"), kind: KindGitHub},
		{name: "GitHub user to server token", literal: ghToken("u"), kind: KindGitHub},
		{name: "GitHub refresh token", literal: ghToken("r"), kind: KindGitHub},
		{name: "GitHub token longer than 36", literal: ghToken("s") + "xyz", kind: KindGitHub},
		{name: "Anthropic API key", literal: anthropic, kind: KindAnthropic},
		{name: "xAI API key", literal: xai, kind: KindXAI},
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
		"const pub = \"-----BEGIN " + "PUBLIC KEY-----\"\n" +
		"const ghs = \"ghs_short\"\n" +
		"const ghx = \"ghx_" + strings.Repeat("a", 36) + "\"\n" +
		"const ant = \"sk-ant-short\"\n" +
		"const xai = \"xai-grok\"\n" +
		"const taxi = \"taxi-" + strings.Repeat("b", 20) + "\"\n"
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

// TestTrackedFilesHaveNoCredentialLiterals fails when a tracked file in this
// module contains a credential-shaped literal. The same check reports a JWT
// written at runtime into a temp copy of a tracked file, and it skips a line
// that contains schmeide:allow-secret.
func TestTrackedFilesHaveNoCredentialLiterals(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	root := moduleRoot(t)
	found, err := Scan(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("tracked files contain credential-shaped literals: %+v", found)
	}

	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(body, []byte("\n")) {
		body = append(body, '\n')
	}
	jwt := "ey" + "JhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	secretLine := bytes.Count(body, []byte("\n")) + 2
	body = append(body, []byte(jwt+" // schmeide:allow-secret\n"+jwt+"\n")...)

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
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "go.mod")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "copy a tracked file")

	found, err = Scan(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Finding{File: "go.mod", Line: secretLine, Kind: KindJWT}
	if len(found) != 1 || found[0] != want {
		t.Fatalf("temp copy findings = %+v, want %+v", found, want)
	}
	if strings.Contains(found[0].Kind, jwt) || strings.Contains(found[0].File, jwt) {
		t.Fatal("finding includes the secret text")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestScanDiffOnAddedLines(t *testing.T) {
	t.Parallel()
	ghs := "gh" + "s_" + strings.Repeat("k", 36)
	nats := "nats-token-without-a-shape"
	tests := []struct {
		name  string
		diff  string
		exact []string
		want  []Finding
	}{
		{
			name: "added line with a shape",
			diff: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3,0 +4,2 @@\n+ok\n+t := \"" + ghs + "\"\n",
			want: []Finding{{File: "a.go", Line: 5, Kind: KindGitHub}},
		},
		{
			name: "removed line is not read",
			diff: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -4 +3,0 @@\n-t := \"" + ghs + "\"\n",
		},
		{
			name: "deleted file is not read",
			diff: "diff --git a/a.go b/a.go\ndeleted file mode 100644\n--- a/a.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-" + ghs + "\n",
		},
		{
			name: "allow marker skips a shape",
			diff: "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1 @@\n+" + ghs + " // schmeide:allow-secret\n",
		},
		{
			name:  "exact credential ignores the allow marker",
			diff:  "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -0,0 +1 @@\n+x = " + nats + " // schmeide:allow-secret\n",
			exact: []string{nats},
			want:  []Finding{{File: "a.go", Line: 1, Kind: KindCredential}},
		},
		{
			name: "added line that looks like a header",
			diff: "diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -0,0 +1,2 @@\n+++ " + ghs + "\n+--- x\n",
			want: []Finding{{File: "a.txt", Line: 1, Kind: KindGitHub}},
		},
		{
			name: "quoted file name",
			diff: "diff --git \"a/caf\\303\\251.txt\" \"b/caf\\303\\251.txt\"\n--- \"a/caf\\303\\251.txt\"\n+++ \"b/caf\\303\\251.txt\"\n@@ -0,0 +1 @@\n+" + ghs + "\n",
			want: []Finding{{File: "caf\u00e9.txt", Line: 1, Kind: KindGitHub}},
		},
		{
			name: "second hunk and second file",
			diff: "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-x\n+y\n@@ -9,0 +10 @@\n+" + ghs + "\n" +
				"diff --git a/b b/b\nnew file mode 100644\n--- /dev/null\n+++ b/b\n@@ -0,0 +1,3 @@\n+a\n+b\n+" + ghs + "\n",
			want: []Finding{{File: "a", Line: 10, Kind: KindGitHub}, {File: "b", Line: 3, Kind: KindGitHub}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var exact [][]byte
			for _, c := range tt.exact {
				exact = append(exact, []byte(c))
			}
			got := scanDiff([]byte(tt.diff), exact)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("scanDiff = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestScanDiffReadsOnlyTheBranchsAddedLines(t *testing.T) {
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
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) {
		t.Helper()
		git("add", "-A")
		git("-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "dev@example.com")
	git("config", "user.name", "Dev")
	old := "AKI" + "AIOSFODNN7EXAMPLE"
	write("legacy.txt", "already here "+old+"\nmore\nlines\nto\nkeep\nrename\nsimilar\n")
	write("other.txt", "one\n")
	commit("base")
	git("checkout", "-q", "-b", "job")

	git("mv", "legacy.txt", "moved.txt")
	write("other.txt", "one\ntwo\n")
	commit("rename and edit")
	got, err := ScanDiff(t.Context(), dir, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("findings for lines already on the base = %+v", got)
	}

	installation := "gh" + "s_" + strings.Repeat("Z", 36)
	nats := "plain-nats-token-value"
	write("other.txt", "one\ntwo\ntoken "+installation+"\nnats="+nats+"\nshort=abc\n")
	commit("leak")
	got, err = ScanDiff(t.Context(), dir, "main", []string{"", "abc", nats})
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{File: "other.txt", Line: 3, Kind: KindGitHub},
		{File: "other.txt", Line: 4, Kind: KindCredential},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ScanDiff = %+v, want %+v", got, want)
	}
	for _, f := range got {
		if strings.Contains(f.File+f.Kind, installation) || strings.Contains(f.File+f.Kind, nats) {
			t.Fatal("finding includes the secret text")
		}
	}

	// A commit that lands on the base after the branch was cut is not read
	// as the branch's own change.
	git("checkout", "-q", "main")
	write("base-only.txt", "later "+installation+"\n")
	commit("base moves on")
	git("checkout", "-q", "job")
	got, err = ScanDiff(t.Context(), dir, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].File != "other.txt" {
		t.Fatalf("ScanDiff after the base moved = %+v, want only other.txt", got)
	}
}

func TestScanDiffRequiresAKnownBase(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	if _, err := ScanDiff(t.Context(), t.TempDir(), "main", nil); err == nil {
		t.Fatal("ScanDiff succeeded on a directory that is not a git worktree")
	}
}
