package runner

import (
	"slices"
	"strings"
)

// baseEnv are the variables every harness session and gate command sees, when
// the runner has them: what a shell, a toolchain, and an HTTPS client need to
// work, and nothing that grants access to anything.
var baseEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// basePrefixes are the variable name prefixes every session and gate sees.
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
