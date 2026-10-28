package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
			for _, w := range []string{"control", "runner", "doctor", "status", "dashboard", "completion"} {
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

// dispatchedCommands returns the command names run's switch dispatches, read
// from main.go so a new command fails TestCompletionListsEveryCommand until
// every script lists it.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sw, ok := n.(*ast.SwitchStmt); ok {
				for _, stmt := range sw.Body.List {
					for _, e := range stmt.(*ast.CaseClause).List {
						if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							name, _ := strconv.Unquote(lit.Value)
							cmds = append(cmds, name)
						}
					}
				}
			}
			return true
		})
	}
	if len(cmds) == 0 {
		t.Fatal("found no commands in run's switch in main.go")
	}
	slices.Sort(cmds)
	return cmds
}

func TestCompletionListsEveryCommand(t *testing.T) {
	want := dispatchedCommands(t)
	extract := map[string]func(script string) []string{
		"bash": func(s string) []string {
			m := regexp.MustCompile(`compgen -W "([^"]*)"`).FindStringSubmatch(s)
			return slices.DeleteFunc(strings.Fields(m[1]), func(w string) bool { return strings.HasPrefix(w, "-") })
		},
		"zsh": func(s string) []string {
			var cmds []string
			for _, m := range regexp.MustCompile(`(?m)^\s+'([a-z]+):`).FindAllStringSubmatch(s, -1) {
				cmds = append(cmds, m[1])
			}
			return cmds
		},
		"fish": func(s string) []string {
			var cmds []string
			for _, m := range regexp.MustCompile(`__fish_use_subcommand -a ([a-z]+) `).FindAllStringSubmatch(s, -1) {
				cmds = append(cmds, m[1])
			}
			return cmds
		},
	}
	for shell, cmds := range extract {
		t.Run(shell, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run([]string{"completion", shell}, &out, &errb); code != 0 {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
			got := cmds(out.String())
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s script lists %v, run dispatches %v", shell, got, want)
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
