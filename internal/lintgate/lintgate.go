// Package lintgate detects a repository's own lint or pre-commit gate.
//
// Given a repository root, it reports a Makefile target named check and a
// pre-commit config when one is present. It does not run those commands, and
// it does not change a configured gate.
package lintgate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const checkTarget = "check"

// Found is the lint or pre-commit gate detected in a repository root.
// A zero Found means nothing was detected.
type Found struct {
	// MakeTarget is "check" when a Makefile defines that target.
	MakeTarget string
	// PreCommit is the pre-commit config file name when one is present.
	PreCommit string
}

// Detect reads root and reports a Makefile target named check and a
// pre-commit config when one is present. A root with neither is a valid
// empty result.
func Detect(root string) (Found, error) {
	info, err := os.Stat(root)
	if err != nil {
		return Found{}, fmt.Errorf("cannot read %s: %w", root, err)
	}
	if !info.IsDir() {
		return Found{}, fmt.Errorf("%s is not a directory", root)
	}
	data, err := makefileBytes(root)
	if err != nil {
		return Found{}, err
	}
	pre, err := preCommitFile(root)
	if err != nil {
		return Found{}, err
	}
	found := Found{PreCommit: pre}
	if hasCheckTarget(data) {
		found.MakeTarget = checkTarget
	}
	return found, nil
}

// Summary is the text doctor reports: what was detected, or that nothing was.
func (f Found) Summary() string {
	var parts []string
	if f.MakeTarget != "" {
		parts = append(parts, "Makefile target `"+f.MakeTarget+"`")
	}
	if f.PreCommit != "" {
		parts = append(parts, "pre-commit config `"+f.PreCommit+"`")
	}
	switch len(parts) {
	case 0:
		return "no Makefile check target or pre-commit config"
	case 1:
		return parts[0]
	default:
		return parts[0] + " and " + parts[1]
	}
}

// Warning is the job comment when gate does not mention a detected target.
// It is empty when nothing was detected or the gate mentions every detected
// target. Posting it does not fail the job.
func Warning(gate string, found Found) string {
	var missed []string
	if found.MakeTarget != "" && !mentions(gate, found.MakeTarget) {
		missed = append(missed, "`"+found.MakeTarget+"`")
	}
	if found.PreCommit != "" && !mentions(gate, "pre-commit") {
		missed = append(missed, "`pre-commit`")
	}
	if len(missed) == 0 {
		return ""
	}
	list := missed[0]
	if len(missed) == 2 {
		list = missed[0] + " or " + missed[1]
	}
	return "schmeide detected " + found.Summary() + ". The configured gate is `" + gate + "`, which does not mention " + list + ". This is a warning; the job still runs."
}

// mentions reports whether command contains target as its own shell word.
func mentions(command, target string) bool {
	if target == "" {
		return false
	}
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '"', '`':
			return -1
		default:
			return r
		}
	}, command)
	for _, word := range strings.FieldsFunc(cleaned, isShellSep) {
		if word == target {
			return true
		}
	}
	return false
}

func isShellSep(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', ';', '|', '&', '(', ')', '<', '>':
		return true
	default:
		return false
	}
}

// makefileBytes reads the makefile GNU make would read: GNUmakefile, then
// makefile, then Makefile. No such file is an empty result.
func makefileBytes(root string) ([]byte, error) {
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		ok, err := regularFile(root, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", path, err)
		}
		return data, nil
	}
	return nil, nil
}

func preCommitFile(root string) (string, error) {
	for _, name := range []string{".pre-commit-config.yaml", ".pre-commit-config.yml"} {
		ok, err := regularFile(root, name)
		if err != nil {
			return "", err
		}
		if ok {
			return name, nil
		}
	}
	return "", nil
}

func regularFile(root, name string) (bool, error) {
	path := filepath.Join(root, name)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return fi.Mode().IsRegular(), nil
}

// hasCheckTarget reports whether data defines a make rule whose target is
// exactly check. Assignments, target-specific variables, and other names
// such as check/go do not count.
func hasCheckTarget(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	defineDepth := 0
	for _, line := range logicalLines(string(data)) {
		stripped := strings.TrimSpace(stripMakeComment(line))
		if defineDepth > 0 {
			switch {
			case stripped == "endef":
				defineDepth--
			case stripped == "define" || strings.HasPrefix(stripped, "define "):
				defineDepth++
			}
			continue
		}
		if stripped == "define" || strings.HasPrefix(stripped, "define ") {
			defineDepth++
			continue
		}
		if line == "" || line[0] == '\t' {
			continue
		}
		for _, name := range ruleTargets(stripped) {
			if name == checkTarget {
				return true
			}
		}
	}
	return false
}

// logicalLines joins backslash-continued lines. A trailing newline does not
// invent an extra line of content beyond the empty string Split already yields.
func logicalLines(data string) []string {
	var lines []string
	var b strings.Builder
	cont := false
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if cont {
			b.WriteByte(' ')
			b.WriteString(strings.TrimLeft(line, " \t"))
		} else {
			b.Reset()
			b.WriteString(line)
		}
		if continued(b.String()) {
			s := b.String()
			b.Reset()
			b.WriteString(s[:len(s)-1])
			cont = true
			continue
		}
		cont = false
		lines = append(lines, b.String())
	}
	if cont {
		lines = append(lines, b.String())
	}
	return lines
}

// continued reports whether s ends in an unescaped backslash.
func continued(s string) bool {
	n := 0
	for n < len(s) && s[len(s)-1-n] == '\\' {
		n++
	}
	return n%2 == 1
}

// stripMakeComment removes a make comment. # starts it, and \# does not.
func stripMakeComment(line string) string {
	for i := range len(line) {
		if line[i] == '#' && (i == 0 || line[i-1] != '\\') {
			return line[:i]
		}
	}
	return line
}

// ruleTargets returns the targets of a make rule. An assignment or a
// target-specific variable is not a rule.
func ruleTargets(line string) []string {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	colon := -1
	depth := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c == '$' && i+1 < len(line) && (line[i+1] == '(' || line[i+1] == '{') {
			depth++
			i++
			continue
		}
		if depth > 0 {
			if c == ')' || c == '}' {
				depth--
			}
			continue
		}
		if (c == '?' || c == '+' || c == '!') && i+1 < len(line) && line[i+1] == '=' {
			return nil
		}
		if c == '=' {
			return nil
		}
		if c != ':' {
			continue
		}
		if i+1 < len(line) && line[i+1] == '=' {
			return nil
		}
		colon = i
		break
	}
	if colon < 0 {
		return nil
	}
	rest := strings.TrimSpace(line[colon+1:])
	if strings.HasPrefix(rest, ":") {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
	}
	if targetSpecific(rest) {
		return nil
	}
	left := strings.TrimSpace(line[:colon])
	if left == "" {
		return nil
	}
	return strings.Fields(left)
}

// targetSpecific reports a suffix of the form "VAR = value" or "VAR=value".
func targetSpecific(rest string) bool {
	if rest == "" {
		return false
	}
	fields := strings.Fields(rest)
	if strings.Contains(fields[0], "=") {
		return true
	}
	if len(fields) < 2 {
		return false
	}
	switch fields[1] {
	case "=", ":=", "?=", "+=", "!=":
		return true
	default:
		return false
	}
}
