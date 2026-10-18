// Package secretscan finds credential-shaped literals in tracked files.
//
// [Scan] reads every tracked file. [ScanDiff] reads only the lines a branch
// adds relative to its base, which is what the runner checks before a push:
// a literal that is already on the base is not the job's to remove. ScanDiff
// also refuses an exact match of a caller-supplied credential, such as the
// job's installation token, which has no shape a pattern could match.
//
// A line containing the comment schmeide:allow-secret is skipped by the
// patterns. The marker has to be on that same line, so a fixture can name a
// shape on purpose. The marker does not excuse an exact credential match.
// The scan reads tracked file contents and git output only. It does not read
// the process environment.
package secretscan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// KindPEM is a PEM private key.
	KindPEM = "PEM private key"
	// KindGitHub is a GitHub token: a personal (ghp_), OAuth (gho_), user
	// to server (ghu_), installation (ghs_), or refresh (ghr_) token, or a
	// fine-grained personal token (github_pat_).
	KindGitHub = "GitHub token"
	// KindGitLab is a GitLab token (glpat-).
	KindGitLab = "GitLab token"
	// KindAWS is an AWS access key (AKIA).
	KindAWS = "AWS access key"
	// KindJWT is a JWT: three base64url segments. The header is base64url
	// JSON, so the token starts with eyJ.
	KindJWT = "JWT"
	// KindAnthropic is an Anthropic API key (sk-ant-).
	KindAnthropic = "Anthropic API key"
	// KindXAI is an xAI API key (xai-).
	KindXAI = "xAI API key"
	// KindCredential is an exact match of a credential the caller named,
	// such as the job's installation token or the runner's NATS token.
	KindCredential = "runner credential"
)

// minCredential is the shortest exact credential ScanDiff looks for. A
// shorter one would match ordinary text.
const minCredential = 8

// allowComment skips the line that contains it.
const allowComment = "schmeide:allow-secret"

// Finding is one credential-shaped literal. It names the file and the kind.
// It does not carry the secret text.
type Finding struct {
	File string
	Line int
	Kind string
}

type pattern struct {
	kind string
	re   *regexp.Regexp
}

// Patterns follow the shapes secret scanners flag. Lengths keep ordinary
// identifiers and short placeholders from matching.
var patterns = []pattern{
	{KindPEM, regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)},
	{KindGitHub, regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{KindGitLab, regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{KindAWS, regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`)},
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{KindAnthropic, regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{KindXAI, regexp.MustCompile(`\bxai-[A-Za-z0-9]{20,}`)},
}

// Scan reads the tracked files in the git worktree at dir and reports
// credential-shaped literals. A line containing schmeide:allow-secret is
// not reported. The process environment is not scanned.
func Scan(ctx context.Context, dir string) ([]Finding, error) {
	names, err := trackedFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	var found []Finding
	for _, name := range names {
		abs := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("read tracked file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read tracked file %s: %w", name, err)
		}
		found = append(found, scan(name, content)...)
	}
	return found, nil
}

func trackedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := gitOutput(ctx, dir, "list tracked files", "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	raw := bytes.Split(out, []byte{0})
	names := make([]string, 0, len(raw))
	for _, name := range raw {
		if len(name) == 0 {
			continue
		}
		names = append(names, string(name))
	}
	return names, nil
}

// ScanDiff reports credential-shaped literals on the lines HEAD adds
// relative to base, in the git worktree at dir. Lines that are already on
// base, including lines a rename carries over unchanged, are not read. A
// line containing schmeide:allow-secret is not reported for its shape. A
// line containing any of credentials verbatim is reported as
// [KindCredential] whatever the line says, and an empty or short credential
// is ignored. The process environment is not scanned.
func ScanDiff(ctx context.Context, dir, base string, credentials []string) ([]Finding, error) {
	// The merge base, so commits base gained after the branch was cut are
	// not read as the branch removing them. -a reads a binary file as text,
	// as Scan does, and -M keeps a renamed file's old lines out of the diff.
	out, err := gitOutput(ctx, dir, "diff the branch against its base",
		"-c", "core.quotePath=false",
		"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-a", "-M", "-U0", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var exact [][]byte
	for _, c := range credentials {
		if len(c) >= minCredential {
			exact = append(exact, []byte(c))
		}
	}
	return scanDiff(out, exact), nil
}

// scanDiff reads unified diff output and reports the first finding on each
// added line.
func scanDiff(diff []byte, exact [][]byte) []Finding {
	var (
		found  []Finding
		file   string
		line   int
		inHunk bool
	)
	for raw := range bytes.SplitSeq(diff, []byte("\n")) {
		switch {
		case bytes.HasPrefix(raw, []byte("diff ")):
			file, inHunk = "", false
		case !inHunk && bytes.HasPrefix(raw, []byte("+++ ")):
			file = diffPath(string(raw[len("+++ "):]))
		case bytes.HasPrefix(raw, []byte("@@ ")):
			line, inHunk = hunkStart(raw), true
		case inHunk && len(raw) > 0 && raw[0] == '+':
			text := bytes.TrimSuffix(raw[1:], []byte("\r"))
			if file != "" {
				if kind, ok := lineKind(text, exact); ok {
					found = append(found, Finding{File: file, Line: line, Kind: kind})
				}
			}
			line++
		}
	}
	return found
}

// lineKind is the finding for one added line: an exact credential first,
// then the patterns unless the line carries the allow marker.
func lineKind(line []byte, exact [][]byte) (string, bool) {
	for _, c := range exact {
		if bytes.Contains(line, c) {
			return KindCredential, true
		}
	}
	if bytes.Contains(line, []byte(allowComment)) {
		return "", false
	}
	return kindOf(line)
}

// diffPath is the file name in a "+++ " header: "b/<name>", a C-quoted
// "\"b/<name>\"", or /dev/null for a deleted file, which names nothing.
func diffPath(header string) string {
	header = strings.TrimSuffix(header, "\t")
	if strings.HasPrefix(header, `"`) {
		if unq, err := strconv.Unquote(header); err == nil {
			header = unq
		}
	}
	if header == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(header, "b/")
}

// hunkStart is the first new-file line number in a "@@ -a,b +c,d @@" header.
func hunkStart(header []byte) int {
	_, rest, ok := bytes.Cut(header, []byte(" +"))
	if !ok {
		return 0
	}
	end := bytes.IndexAny(rest, ", ")
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.Atoi(string(rest[:end]))
	if err != nil {
		return 0
	}
	return n
}

// gitOutput runs git in dir and returns its standard output. what names the
// step in an error.
func gitOutput(ctx context.Context, dir, what string, args ...string) ([]byte, error) {
	gitBin, err := findGit()
	if err != nil {
		return nil, err
	}
	// The absolute binary, not PATH's first git: a test may prepend a wrapper
	// that is not git. The environment is not the process environment, so a
	// runner secret is not handed to git and is not something the scan reads.
	cmd := exec.CommandContext(ctx, gitBin, append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	}
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			detail = strings.TrimSpace(string(exit.Stderr))
		}
		if detail == "" {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		return nil, fmt.Errorf("%s: %w: %s", what, err, detail)
	}
	return out, nil
}

// findGit returns a git binary. A shell wrapper earlier on PATH is skipped
// so ls-files runs the real git.
func findGit() (string, error) {
	var fallback string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, "git")
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		script, err := shebang(candidate)
		if err != nil {
			continue
		}
		if script {
			if fallback == "" {
				fallback = candidate
			}
			continue
		}
		return candidate, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("git was not found on PATH; install git")
}

func shebang(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var buf [2]byte
	n, err := f.Read(buf[:])
	if n >= 2 {
		return buf[0] == '#' && buf[1] == '!', nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return false, nil
}

// scan reports the first credential shape on each line of one file.
func scan(file string, content []byte) []Finding {
	lines := bytes.Split(content, []byte("\n"))
	var found []Finding
	for i, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.Contains(line, []byte(allowComment)) {
			continue
		}
		kind, ok := kindOf(line)
		if !ok {
			continue
		}
		found = append(found, Finding{File: file, Line: i + 1, Kind: kind})
	}
	return found
}

func kindOf(line []byte) (string, bool) {
	for _, p := range patterns {
		if p.re.Match(line) {
			return p.kind, true
		}
	}
	return "", false
}
