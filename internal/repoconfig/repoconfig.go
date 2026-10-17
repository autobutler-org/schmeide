// Package repoconfig loads and validates the schmeide config tracked in a
// target repo at [Path]. It has no dependency on the runner, so the control
// node and the CLI can both use it.
//
// # Schema
//
// The file is TOML. Unknown keys are an error so typos do not pass silently.
//
//	# .schmeide/config.toml
//
//	# Required. Shell command a job must pass before a PR is opened.
//	gate = "make check test"
//
//	# Optional. How long factory merge waits for the head commit's check
//	# runs to succeed. A Go duration. Absent or empty means 2m. Only read
//	# when automate.merge = "on".
//	check_wait = "10m"
//
//	# Optional. How long the gate command may run. A Go duration.
//	# Absent or empty keeps the runner's --gate-timeout, which defaults to 30m.
//	gate_timeout = "30m"
//
//	# Optional. How long one harness session may run. A Go duration.
//	# Absent or empty keeps the runner's --session-timeout, which defaults to 2h.
//	session_timeout = "2h"
//
//	# Optional. What the factory does without a person. Each switch is
//	# "on" or "off". Absent, empty, and blank mean "off".
//	[automate]
//	# "on" merges after review has passed and CI is green, with one writer
//	# fix round for a red check and stacked pull requests restacked.
//	merge = "on"
//	# "on" gives the writer fix rounds for review findings. "off" leaves a
//	# pull request whose review did not pass for a person.
//	review_fix = "on"
//	# Accepted ahead of the feature. Nothing reads it yet.
//	create_issues = "off"
package repoconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Path is the config file location relative to the repo root.
const Path = ".schmeide/config.toml"

const (
	// DefaultCheckWait is the check wait when check_wait is absent or empty.
	DefaultCheckWait = 2 * time.Minute
	// DefaultGateTimeout is the gate deadline when gate_timeout is absent or
	// empty and the runner did not set --gate-timeout. The flag's own default
	// is this value.
	DefaultGateTimeout = 30 * time.Minute
	// DefaultSessionTimeout is the session deadline when session_timeout is
	// absent or empty and the runner did not set --session-timeout. The
	// flag's own default is this value.
	DefaultSessionTimeout = 2 * time.Hour
)

// Config is the per-repo schmeide configuration.
type Config struct {
	// Gate is the command a job must pass before a PR is opened.
	Gate string `toml:"gate"`
	// Automate is the [automate] table. Every switch defaults to off.
	Automate Automate `toml:"-"`
	// CheckWait is how long factory merge waits for the head commit's check
	// runs. Parse sets it to [DefaultCheckWait] when the key is absent.
	CheckWait time.Duration `toml:"-"`
	// GateTimeout is how long one gate command may run. Parse sets it to
	// [DefaultGateTimeout] when gate_timeout is absent or empty. A runner
	// flag overrides that absent case; use [Config.GateTimeoutOr].
	GateTimeout time.Duration `toml:"-"`
	// SessionTimeout is how long one harness session may run. Parse sets it
	// to [DefaultSessionTimeout] when session_timeout is absent or empty. A
	// runner flag overrides that absent case; use [Config.SessionTimeoutOr].
	SessionTimeout time.Duration `toml:"-"`
	// gateTimeoutSet is true when gate_timeout was a positive duration.
	gateTimeoutSet bool
	// sessionTimeoutSet is true when session_timeout was a positive duration.
	sessionTimeoutSet bool
}

// Automate holds the [automate] switches. A true field is "on".
type Automate struct {
	// Merge has the runner merge a pull request once review has passed and
	// every check run on the head commit has succeeded. It covers the one
	// writer fix round for a red check and restacking stacked pull requests.
	// Off leaves the pull request for a person.
	Merge bool
	// ReviewFix gives the writer fix rounds for review findings. Off still
	// runs the reviewer, and a review that does not pass leaves the pull
	// request open for a person.
	ReviewFix bool
	// CreateIssues is accepted ahead of the feature. Nothing reads it yet.
	CreateIssues bool
}

// Switch values for the [automate] table.
const (
	On  = "on"
	Off = "off"
)

// file is the TOML shape of [Config].
type file struct {
	Gate           string       `toml:"gate"`
	CheckWait      string       `toml:"check_wait"`
	GateTimeout    string       `toml:"gate_timeout"`
	SessionTimeout string       `toml:"session_timeout"`
	Automate       automateFile `toml:"automate"`
}

// automateFile is the TOML shape of [Automate].
type automateFile struct {
	Merge        string `toml:"merge"`
	ReviewFix    string `toml:"review_fix"`
	CreateIssues string `toml:"create_issues"`
}

// examples holds one valid value per key, for error messages.
var examples = map[string]string{
	"gate":                   "make check test",
	"check_wait":             "10m",
	"gate_timeout":           "30m",
	"session_timeout":        "30m",
	"automate.merge":         On,
	"automate.review_fix":    On,
	"automate.create_issues": On,
}

// nonStringKey reports the first known key whose value is not a string, when
// data is otherwise valid TOML. An automate key is named with its table.
func nonStringKey(data []byte) (string, bool) {
	var m map[string]any
	if toml.Unmarshal(data, &m) != nil {
		return "", false
	}
	for _, key := range []string{"gate", "check_wait", "gate_timeout", "session_timeout"} {
		if v, ok := m[key]; ok {
			if _, isString := v.(string); !isString {
				return key, true
			}
		}
	}
	automate, ok := m["automate"]
	if !ok {
		return "", false
	}
	table, isTable := automate.(map[string]any)
	if !isTable {
		return "automate", true
	}
	for _, key := range []string{"merge", "review_fix", "create_issues"} {
		if v, ok := table[key]; ok {
			if _, isString := v.(string); !isString {
				return "automate." + key, true
			}
		}
	}
	return "", false
}

// parseSwitch reads one [automate] switch. Empty and blank are off. Any
// value other than "on" or "off" is an error that names the key.
func parseSwitch(name, key, raw string) (bool, error) {
	switch {
	case raw == On:
		return true, nil
	case raw == Off, strings.TrimSpace(raw) == "":
		return false, nil
	}
	return false, fmt.Errorf("%s: %s = %q is not a legal value; set %s = %q or %s = %q", name, key, raw, key, On, key, Off)
}

// GateTimeoutOr returns the repo's gate timeout when gate_timeout is set,
// and fallback when the key is absent or empty. A non-positive fallback
// selects [DefaultGateTimeout], which is 30 minutes.
func (c Config) GateTimeoutOr(fallback time.Duration) time.Duration {
	if c.gateTimeoutSet {
		return c.GateTimeout
	}
	if fallback > 0 {
		return fallback
	}
	return DefaultGateTimeout
}

// SessionTimeoutOr returns the repo's session timeout when session_timeout
// is set, and fallback when the key is absent or empty. A non-positive
// fallback selects [DefaultSessionTimeout], which is 2 hours.
func (c Config) SessionTimeoutOr(fallback time.Duration) time.Duration {
	if c.sessionTimeoutSet {
		return c.SessionTimeout
	}
	if fallback > 0 {
		return fallback
	}
	return DefaultSessionTimeout
}

// parsePositiveDuration parses a Go duration. An empty or blank raw value
// is unset. Anything else that is not a positive duration is an error that
// names key and gives 30m as an example.
func parsePositiveDuration(name, key, raw string) (time.Duration, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return 0, false, fmt.Errorf("%s: %s = %q is not a positive duration; set a duration such as %s = \"30m\"", name, key, raw, key)
	}
	return d, true, nil
}

// Load reads and validates the config in the repo rooted at repoDir.
func Load(repoDir string) (Config, error) {
	file := filepath.Join(repoDir, filepath.FromSlash(Path))
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: config file not found; create it with a gate command, e.g. gate = \"make check test\"", file)
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: cannot read config file: %w", file, err)
	}
	return Parse(file, data)
}

// Parse validates config bytes. name is used in error messages.
func Parse(name string, data []byte) (Config, error) {
	var raw file
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			keys := make([]string, 0, len(strict.Errors))
			for _, e := range strict.Errors {
				key := strings.Join(e.Key(), ".")
				if key == "merge" {
					return Config{}, fmt.Errorf("%s: merge moved into [automate]; set it as [automate] merge = %q", name, On)
				}
				keys = append(keys, key)
			}
			return Config{}, fmt.Errorf("%s: unknown key(s) %s; remove them or fix the spelling", name, strings.Join(keys, ", "))
		}
		if key, ok := nonStringKey(data); ok {
			if key == "automate" {
				return Config{}, fmt.Errorf("%s: automate must be a table; set it like [automate] merge = %q", name, On)
			}
			return Config{}, fmt.Errorf("%s: %s must be a quoted string; set it like %s = %q", name, key, key, examples[key])
		}
		return Config{}, fmt.Errorf("%s: malformed TOML, fix the syntax: %w", name, err)
	}
	cfg := Config{
		Gate:           raw.Gate,
		CheckWait:      DefaultCheckWait,
		GateTimeout:    DefaultGateTimeout,
		SessionTimeout: DefaultSessionTimeout,
	}
	if s := strings.TrimSpace(raw.CheckWait); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s: check_wait = %q is not a positive duration; set a duration such as check_wait = \"10m\"", name, raw.CheckWait)
		}
		cfg.CheckWait = d
	}
	gateTimeout, gateSet, err := parsePositiveDuration(name, "gate_timeout", raw.GateTimeout)
	if err != nil {
		return Config{}, err
	}
	if gateSet {
		cfg.GateTimeout = gateTimeout
		cfg.gateTimeoutSet = true
	}
	sessionTimeout, sessionSet, err := parsePositiveDuration(name, "session_timeout", raw.SessionTimeout)
	if err != nil {
		return Config{}, err
	}
	if sessionSet {
		cfg.SessionTimeout = sessionTimeout
		cfg.sessionTimeoutSet = true
	}
	if strings.TrimSpace(cfg.Gate) == "" {
		return Config{}, fmt.Errorf("%s: gate is missing or empty; set gate = \"<command>\", e.g. gate = \"make check test\"", name)
	}
	merge, err := parseSwitch(name, "automate.merge", raw.Automate.Merge)
	if err != nil {
		return Config{}, err
	}
	reviewFix, err := parseSwitch(name, "automate.review_fix", raw.Automate.ReviewFix)
	if err != nil {
		return Config{}, err
	}
	createIssues, err := parseSwitch(name, "automate.create_issues", raw.Automate.CreateIssues)
	if err != nil {
		return Config{}, err
	}
	cfg.Automate = Automate{Merge: merge, ReviewFix: reviewFix, CreateIssues: createIssues}
	return cfg, nil
}
