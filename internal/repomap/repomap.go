// Package repomap builds a short map of a git checkout for a harness prompt.
//
// The map lists tracked files. Go files also list their top-level function
// and type names. The text is capped so a large tree cannot crowd out the
// brief.
package repomap

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	// maxBytes is the cap on the map text.
	maxBytes = 8 * 1024

	// truncatedNote is the last line of a map that did not fit in maxBytes.
	truncatedNote = "This map was truncated.\n"

	// header is the first line of a non-empty map.
	header = "Repository map:\n"
)

// Map returns a short map of the tracked files in dir.
//
// Go files list their top-level function and type names. The text is at
// most 8 KiB. A map that would exceed that cap is truncated and says it
// was truncated. A directory that is not a git checkout returns empty
// text and no error.
func Map(dir string) (string, error) {
	names, err := tracked(dir)
	if err != nil || len(names) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString(header)
	for _, name := range names {
		b.WriteString(render(dir, name))
	}
	if b.Len() == len(header) {
		return "", nil
	}
	return capText(b.String()), nil
}

// tracked lists the files git tracks in dir. A directory that is not a
// checkout returns no names and no error.
func tracked(dir string) ([]string, error) {
	cmd := exec.Command("git", "-C", dir, "ls-files", "-z")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	}
	if home := os.Getenv("HOME"); home != "" {
		cmd.Env = append(cmd.Env, "HOME="+home)
	}
	out, err := cmd.Output()
	if err != nil {
		if notCheckout(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list tracked files: %w", err)
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

// notCheckout reports whether err is git refusing a directory that is not
// a checkout.
func notCheckout(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	return bytes.Contains(bytes.ToLower(exit.Stderr), []byte("not a git repository"))
}

// render is one tracked file: its path, and for a Go file the top-level
// function and type names. An unsafe path is skipped.
func render(dir, name string) string {
	abs, ok := locate(dir, name)
	if !ok {
		return ""
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('\n')
	if !strings.HasSuffix(name, ".go") {
		return b.String()
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return b.String()
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return b.String()
	}
	for _, line := range goSymbols(src) {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// locate joins name onto dir when name is a relative path that stays inside
// dir.
func locate(dir, name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, "\n\r") {
		return "", false
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if rel == "." || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(dir, rel), true
}

// goSymbols lists top-level function and type names in src. A file that
// does not parse still contributes the names in the partial syntax tree.
func goSymbols(src []byte) []string {
	file, _ := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if file == nil {
		return nil
	}
	var lines []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if fn := identName(d.Name); fn != "" {
				lines = append(lines, "  func "+fn)
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if typ := identName(ts.Name); typ != "" {
					lines = append(lines, "  type "+typ)
				}
			}
		}
	}
	return lines
}

func identName(id *ast.Ident) string {
	if id == nil || id.Name == "" || id.Name == "_" {
		return ""
	}
	return id.Name
}

// capText returns text unchanged when it fits. Otherwise it keeps a leading
// prefix and says the map was truncated. The result is at most maxBytes.
func capText(text string) string {
	if len(text) <= maxBytes {
		return text
	}
	budget := maxBytes - len(truncatedNote)
	cut := text
	if len(cut) > budget {
		cut = cut[:budget]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i+1]
	}
	return cut + truncatedNote
}
