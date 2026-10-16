package runner

import (
	"path/filepath"
	"strings"
	"sync"
)

// goCacher is the optional [Workspace] method that gives a job's harness
// session the same Go caches its gate uses.
type goCacher interface {
	// GoCacheEnv returns env with the Go build and module cache settings
	// added.
	GoCacheEnv(env []string) []string
}

// openWorkspaces maps a workspace's Dir to the workspace, so that
// [Runner.session], which is handed only the directory, can find it.
var openWorkspaces sync.Map

// goCacheEnv points GOCACHE and GOMODCACHE at dir. The module cache is
// written writable, so removing the work directory removes it too.
func goCacheEnv(env []string, dir string) []string {
	flags := "-modcacherw"
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, "GOFLAGS="); ok && v != "" {
			flags = v + " " + flags
		}
	}
	return append(env,
		"GOCACHE="+filepath.Join(dir, "go-build"),
		"GOMODCACHE="+filepath.Join(dir, "pkg", "mod"),
		"GOFLAGS="+flags,
	)
}
