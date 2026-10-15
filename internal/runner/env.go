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

// harnessPrefixes are what a harness session sees on top of the base: the
// harness's own login and settings, which it needs to run at all. XAI_ is
// the Grok CLI login (XAI_API_KEY). The gate does not see these prefixes.
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

// jobEnv is the environment of a job's git commands and gate. It leaves out
// everything the runner was given that the job's code must not see, the NATS
// token among them.
func jobEnv(environ, passEnv []string) []string {
	return filterEnv(environ, slices.Concat(baseEnv, passEnv), basePrefixes)
}

// harnessEnv is the environment of a job's harness session: the job's
// environment plus the harness's own settings.
func harnessEnv(environ, passEnv []string) []string {
	return filterEnv(environ, slices.Concat(baseEnv, passEnv), slices.Concat(basePrefixes, harnessPrefixes))
}
