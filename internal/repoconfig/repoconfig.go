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
//	# Optional. Who merges after review has passed and CI is green.
//	# Absent, empty, and "human" leave the pull request for a person.
//	# "factory" means the factory merges.
//	merge = "human"
//
//	# Optional. How long factory merge waits for the head commit's check
//	# runs to succeed. A Go duration. Absent or empty means 2m.
//	check_wait = "10m"
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
	// MergeHuman leaves the pull request for a person. An absent or empty
	// merge key means the same thing.
	MergeHuman = "human"
	// MergeFactory merges after review has passed and every check run on the
	// head commit has succeeded.
	MergeFactory = "factory"
)

// DefaultCheckWait is the check wait when check_wait is absent or empty.
const DefaultCheckWait = 2 * time.Minute

// Config is the per-repo schmeide configuration.
type Config struct {
	// Gate is the command a job must pass before a PR is opened.
	Gate string `toml:"gate"`
	// Merge is who merges a pull request that passed review once CI is green.
	// Empty and [MergeHuman] leave it for a person. [MergeFactory] means the
	// runner merges.
	Merge string `toml:"merge"`
	// CheckWait is how long factory merge waits for the head commit's check
	// runs. Parse sets it to [DefaultCheckWait] when the key is absent.
	CheckWait time.Duration `toml:"-"`
}

// file is the TOML shape of [Config].
type file struct {
	Gate      string `toml:"gate"`
	Merge     string `toml:"merge"`
	CheckWait string `toml:"check_wait"`
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
				keys = append(keys, strings.Join(e.Key(), "."))
			}
			return Config{}, fmt.Errorf("%s: unknown key(s) %s; remove them or fix the spelling", name, strings.Join(keys, ", "))
		}
		return Config{}, fmt.Errorf("%s: malformed TOML, fix the syntax: %w", name, err)
	}
	cfg := Config{Gate: raw.Gate, Merge: raw.Merge, CheckWait: DefaultCheckWait}
	if s := strings.TrimSpace(raw.CheckWait); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s: check_wait = %q is not a positive duration; set a duration such as check_wait = \"10m\"", name, raw.CheckWait)
		}
		cfg.CheckWait = d
	}
	if strings.TrimSpace(cfg.Gate) == "" {
		return Config{}, fmt.Errorf("%s: gate is missing or empty; set gate = \"<command>\", e.g. gate = \"make check test\"", name)
	}
	switch cfg.Merge {
	case "", MergeHuman, MergeFactory:
	default:
		if strings.TrimSpace(cfg.Merge) == "" {
			cfg.Merge = ""
			break
		}
		return Config{}, fmt.Errorf("%s: merge = %q is not a legal value; set merge = %q or merge = %q", name, cfg.Merge, MergeFactory, MergeHuman)
	}
	return cfg, nil
}
