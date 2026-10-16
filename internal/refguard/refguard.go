// Package refguard keeps a job worktree from moving other jobs' branches.
package refguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the hooks directory Install writes inside the worktree.
const Dir = ".schmeide-refguard"

// hook is a reference-transaction hook. Git runs it with "prepared" before it
// changes any ref, and a non-zero exit aborts the whole transaction. Job
// branches are named <issue number>_<slug>, so any refs/heads/<digits>_* ref
// other than own is refused: created, moved or deleted.
const hook = `#!/bin/sh
# Written by schmeide: this job worktree may move only its own branch.
[ "$1" = prepared ] || exit 0
own=%s
status=0
while read -r old new ref; do
	[ "$ref" = "$own" ] && continue
	case $ref in
	refs/heads/[0-9]*_*)
		branch=${ref#refs/heads/}
		case ${branch%%%%_*} in
		*[!0-9]*) ;;
		*)
			echo "schmeide: this job worktree may move only $own, not $ref" >&2
			status=1
			;;
		esac
		;;
	esac
done
exit $status
`

// Install writes the hook for branch into Dir under worktree and returns the
// hooks directory's absolute path, for the worktree's core.hooksPath. The
// directory ignores itself, so committing everything in the worktree does
// not pick it up.
func Install(worktree, branch string) (string, error) {
	dir, err := filepath.Abs(filepath.Join(worktree, Dir))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create the ref guard hooks directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o600); err != nil {
		return "", fmt.Errorf("write the ref guard ignore file: %w", err)
	}
	script := fmt.Sprintf(hook, shellQuote("refs/heads/"+branch))
	if err := os.WriteFile(filepath.Join(dir, "reference-transaction"), []byte(script), 0o755); err != nil {
		return "", fmt.Errorf("write the ref guard hook: %w", err)
	}
	return dir, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
