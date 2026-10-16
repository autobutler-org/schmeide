package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/repoconfig"
)

const initUsage = `usage: schmeide init

Write ` + repoconfig.Path + ` (the repo config) and .schmeide/control.conf (the
control node config) in the current directory. Refuses to overwrite either.
`

const initRepoConfig = `# schmeide repo config. See docs/config.md for every key.
# merge, check_wait, gate_timeout, and session_timeout are unset: defaults apply.

# Shell command a job must pass before a PR is opened.
gate = "make check test"
`

const initControlPath = ".schmeide/control.conf"

func initMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, initUsage)
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintf(stderr, "schmeide init: unexpected argument %q\n%s", args[0], initUsage)
		return 2
	}
	files := []struct{ path, content string }{
		{repoconfig.Path, initRepoConfig},
		{initControlPath, control.InitConfig},
	}
	for _, f := range files {
		if _, err := os.Lstat(f.path); err == nil {
			fmt.Fprintf(stderr, "schmeide init: %s already exists; remove it or edit it by hand, nothing was changed\n", f.path)
			return 1
		} else if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "schmeide init: %v\n", err)
			return 1
		}
	}
	if err := os.MkdirAll(filepath.Dir(repoconfig.Path), 0o755); err != nil {
		fmt.Fprintf(stderr, "schmeide init: %v\n", err)
		return 1
	}
	for _, f := range files {
		// Control config holds secrets once filled in, so keep it private.
		out, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = out.WriteString(f.content)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			fmt.Fprintf(stderr, "schmeide init: write %s: %v\n", f.path, err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %s\n", f.path)
	}
	fmt.Fprintln(stdout, "Next: set the gate in "+repoconfig.Path+", and replace every replace-me in "+initControlPath+".")
	return 0
}
