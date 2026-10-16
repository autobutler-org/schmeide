// Package trust records which pairs of work directory and origin URL the
// operator has agreed to let a runner work. Decisions live under the user
// config directory, outside any worktree a job could write to.
package trust

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// EnvVar set to 1 trusts every pair for the life of the process without
// recording anything.
const EnvVar = "SCHMEIDE_TRUST"

// Check returns nil when dir and origin may be worked: SCHMEIDE_TRUST=1 is
// set, a yes is recorded for the pair, or confirm answers yes, which is then
// recorded. A nil confirm means nobody can be asked, and an unknown pair is
// refused with an error that says how to proceed.
func Check(dir, origin string, confirm func(question string) (bool, error)) error {
	if os.Getenv(EnvVar) == "1" {
		return nil
	}
	file, err := path(dir, origin)
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the trust decision: %w", err)
	}
	canon := canonical(dir)
	if confirm == nil {
		return fmt.Errorf("work directory %s has not been trusted for %s; run the runner on a terminal once to answer, or set %s=1 to trust it for this run", canon, origin, EnvVar)
	}
	ok, err := confirm(fmt.Sprintf("Let schmeide run agent sessions and gate commands from %s in %s?", origin, canon))
	if err != nil {
		return fmt.Errorf("asking for trust: %w", err)
	}
	if !ok {
		return fmt.Errorf("work directory %s was not trusted for %s; restart the runner to be asked again, or set %s=1", canon, origin, EnvVar)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return fmt.Errorf("recording the trust decision: %w", err)
	}
	if err := os.WriteFile(file, []byte(canon+"\n"+origin+"\n"), 0o600); err != nil {
		return fmt.Errorf("recording the trust decision: %w", err)
	}
	return nil
}

// path is the file that records a yes for the pair.
func path(dir, origin string) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the user config directory for trust decisions: %w; set HOME or XDG_CONFIG_HOME", err)
	}
	sum := sha256.Sum256([]byte(canonical(dir) + "\x00" + origin))
	return filepath.Join(base, "schmeide", "trust", hex.EncodeToString(sum[:])), nil
}

// canonical resolves dir to an absolute path without symlinks. A directory
// that does not exist yet keeps its cleaned absolute path.
func canonical(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}
