// Package secretscan finds credential-shaped literals in tracked files.
//
// A line containing the comment schmeide:allow-secret is skipped. The marker
// has to be on that same line, so a fixture can name a shape on purpose.
// The scan reads tracked file contents only. It does not read the process
// environment.
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
	"strings"
)

const (
	// KindPEM is a PEM private key.
	KindPEM = "PEM private key"
	// KindGitHub is a GitHub token (ghp_ or github_pat_).
	KindGitHub = "GitHub token"
	// KindGitLab is a GitLab token (glpat-).
	KindGitLab = "GitLab token"
	// KindAWS is an AWS access key (AKIA).
	KindAWS = "AWS access key"
	// KindJWT is a JWT: three base64url segments. The header is base64url
	// JSON, so the token starts with eyJ.
	KindJWT = "JWT"
)

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
	{KindGitHub, regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{KindGitLab, regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{KindAWS, regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`)},
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
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
	gitBin, err := findGit()
	if err != nil {
		return nil, err
	}
	// The absolute binary, not PATH's first git: a test may prepend a wrapper
	// that is not git. The environment is not the process environment, so a
	// runner secret is not handed to git and is not something the scan reads.
	cmd := exec.CommandContext(ctx, gitBin, "-C", dir, "ls-files", "-z")
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
			return nil, fmt.Errorf("list tracked files: %w", err)
		}
		return nil, fmt.Errorf("list tracked files: %w: %s", err, detail)
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
