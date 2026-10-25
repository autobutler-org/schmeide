package runner

import (
	"slices"
	"strings"
)

// baseEnv are the variables every harness session sees, when the runner has
// them: what a shell, a toolchain, and an HTTPS client need to work, and
// nothing that grants access to anything. The gate does not use this list: it
// inherits the runner's whole environment (see "The gate inherits the runner's
// environment" in docs/architecture/credentials.md, and issue #111).
var baseEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// basePrefixes are the variable name prefixes every harness session sees.
var basePrefixes = []string{"LC_", "XDG_"}

// harnessPrefixes are added for the harness session only, on top of the
// base: the harness's own login and settings, which it needs to run at all.
// XAI_ is the Grok CLI login (XAI_API_KEY).
var harnessPrefixes = []string{"ANTHROPIC_", "CLAUDE_", "XAI_"}

// filterEnv keeps the entries of environ whose names are in names or start
// with one of prefixes.
func filterEnv(environ, names, prefixes []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return !slices.Contains(names, name) &&
			!slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
	})
}

// harnessEnv is the environment of a job's harness session: the job's
// environment plus the harness's own settings.
func harnessEnv(environ, passEnv []string) []string {
	return filterEnv(environ, slices.Concat(baseEnv, passEnv), slices.Concat(basePrefixes, harnessPrefixes))
}

// gateEnv is the environment of a job's gate command: the base list, the
// base prefixes, and passEnv, and never a SCHMEIDE_ name, even one listed in
// passEnv. HOME and TMPDIR are replaced per job by [scratchHome].
func gateEnv(environ, passEnv []string) []string {
	return slices.DeleteFunc(filterEnv(environ, slices.Concat(baseEnv, passEnv), basePrefixes), func(entry string) bool {
		return strings.HasPrefix(entry, "SCHMEIDE_")
	})
}

// homeEnv are the variables that point a process at the runner's own home.
// [scratchHome] drops them, so the XDG directories default to under the
// scratch HOME.
var homeEnv = []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"}

// scratchHome returns env with HOME and TMPDIR pointed at dir, so the gate
// reads no dotfiles from the runner's home and leaves nothing behind once
// dir is removed.
func scratchHome(env []string, dir string) []string {
	env = slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(homeEnv, name)
	})
	return append(env, "HOME="+dir, "TMPDIR="+dir)
}
