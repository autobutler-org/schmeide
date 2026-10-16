// Package doctor checks that a machine is ready to run `schmeide runner` and
// says how to fix whatever is not. One working harness is enough. A harness
// the runner will not use is reported as not selected.
package doctor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/runner"
)

// Groups say who fixes a failed check.
const (
	// GroupOperator checks are fixed by the person running doctor: a login,
	// a flag, a token.
	GroupOperator = "operator"
	// GroupMachine checks are fixed on the machine itself: installing a
	// tool, freeing disk.
	GroupMachine = "machine"
)

// minFreeDisk is the free space below which the disk counts as full.
const minFreeDisk = 1 << 30

// Check is one thing doctor looks at.
type Check struct {
	Name string
	// Group is [GroupOperator] or [GroupMachine].
	Group string
	// Harness is true when this check is one harness. Those checks pass as
	// a group when at least one of them passes.
	Harness bool
	// Run returns a short description of what it found, or an error whose
	// text says how to fix the problem.
	Run func(ctx context.Context) (string, error)
	// Fix, when set, repairs a failed Run without guessing and without
	// losing data. --fix calls it, then runs the check again.
	Fix func() error
}

// Checks returns the checks for a runner that would use the given control
// node and harnesses.
func Checks(natsURL, steward, natsToken string, harnesses []harness.Harness) []Check {
	checks := []Check{{Name: "git", Group: GroupMachine, Run: func(context.Context) (string, error) {
		path, err := exec.LookPath("git")
		if err != nil {
			return "", errors.New("git was not found on PATH; install it (https://git-scm.com/downloads) and try again")
		}
		return path, nil
	}}, GHCheck(), DiskCheck(".", minFreeDisk)}
	for _, h := range harnesses {
		checks = append(checks, Check{Name: "harness " + h.Name(), Group: GroupOperator, Harness: true, Run: func(ctx context.Context) (string, error) {
			if err := h.Check(ctx); err != nil {
				return "", err
			}
			if v := harnessVersion(ctx, h); v != "" {
				return "installed and logged in, version " + v, nil
			}
			return "installed and logged in", nil
		}})
	}
	return append(checks, Check{Name: "control node", Group: GroupOperator, Run: func(ctx context.Context) (string, error) {
		if steward == "" {
			return "", errors.New("no steward to connect as; pass your GitHub login with --steward")
		}
		nc, _, err := runner.Connect(ctx, natsURL, steward, natsToken)
		if err != nil {
			return "", err
		}
		nc.Close()
		return "reachable at " + natsURL, nil
	}})
}

// GHCheck checks that the GitHub CLI is installed and logged in.
func GHCheck() Check {
	return Check{Name: "gh", Group: GroupMachine, Run: func(ctx context.Context) (string, error) {
		path, err := exec.LookPath("gh")
		if err != nil {
			return "", errors.New("gh was not found on PATH; install the GitHub CLI (https://cli.github.com) and try again")
		}
		if out, err := exec.CommandContext(ctx, path, "auth", "status").CombinedOutput(); err != nil {
			return "", fmt.Errorf("gh is not logged in (%s); run 'gh auth login' and try again", firstLine(string(out), err))
		}
		return path + " is logged in", nil
	}}
}

// DiskCheck checks that the disk holding dir has at least minFree bytes
// available to this process.
func DiskCheck(dir string, minFree uint64) Check {
	return Check{Name: "disk", Group: GroupMachine, Run: func(context.Context) (string, error) {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			return "", fmt.Errorf("cannot read free space of %s: %w", dir, err)
		}
		free := uint64(st.Bavail) * uint64(st.Bsize) //nolint:gosec,unconvert // field types differ by OS
		if free < minFree {
			return "", fmt.Errorf("the disk holding %s is full: %d MiB free, want at least %d MiB; free some space and try again", dir, free>>20, minFree>>20)
		}
		return fmt.Sprintf("%d MiB free", free>>20), nil
	}}
}

// harnessVersion returns the first line of the harness CLI's --version, or
// "" when it has none to give.
func harnessVersion(ctx context.Context, h harness.Harness) string {
	var bin string
	switch h := h.(type) {
	case *harness.ClaudeCode:
		bin = cmp.Or(h.Bin, "claude")
	case *harness.Grok:
		bin = cmp.Or(h.Bin, "grok")
	default:
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return firstLine(string(out), nil)
}

// firstLine returns the first non-empty line of s, or err's text when s has
// none.
func firstLine(s string, err error) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// Result is what one check found. Its JSON form is the stable shape of
// `schmeide doctor --json`.
type Result struct {
	Name   string `json:"name"`
	Group  string `json:"group"`
	Status string `json:"status"` // "ok", "fail", or "skip"
	Detail string `json:"detail"`
}

// Run runs the checks and reports whether this machine is ready, along with
// the names of checks it fixed. Git and the control node fail on their own.
// Harness checks pass when at least one of them passes; each other harness
// is skipped as not selected, with the fix its error carries. When every
// harness fails, each one fails. With fix, a failed check that has a Fix is
// fixed and run again.
func Run(ctx context.Context, checks []Check, fix bool) (results []Result, ok bool, fixed []string) {
	errs := make([]error, len(checks))
	results = make([]Result, len(checks))
	// Run every check before judging. A harness failure is not selected
	// only once some other harness has passed, which may come later.
	harnessPassed := false
	for i, c := range checks {
		found, err := c.Run(ctx)
		if err != nil && fix && c.Fix != nil {
			if err = c.Fix(); err == nil {
				fixed = append(fixed, c.Name)
				found, err = c.Run(ctx)
			}
		}
		results[i], errs[i] = Result{Name: c.Name, Group: c.Group, Status: "ok", Detail: found}, err
		if c.Harness && err == nil {
			harnessPassed = true
		}
	}
	ok = true
	for i, err := range errs {
		switch {
		case err == nil:
		case checks[i].Harness && harnessPassed:
			results[i].Status, results[i].Detail = "skip", "not selected: "+err.Error()
		default:
			ok = false
			results[i].Status, results[i].Detail = "fail", err.Error()
		}
	}
	return results, ok, fixed
}

// Report runs the checks, writes them to w grouped by who fixes them, and
// reports whether this machine is ready. See [Run].
func Report(ctx context.Context, w io.Writer, checks []Check) bool {
	results, ok, _ := Run(ctx, checks, false)
	writeText(w, results)
	return ok
}

func writeText(w io.Writer, results []Result) {
	label := map[string]string{"ok": "ok  ", "fail": "FAIL", "skip": "skip"}
	for _, g := range []string{GroupOperator, GroupMachine, ""} {
		header := false
		for _, r := range results {
			if r.Group != g {
				continue
			}
			if !header && g != "" {
				fmt.Fprintf(w, "%s:\n", g)
			}
			header = true
			fmt.Fprintf(w, "  %s  %s: %s\n", label[r.Status], r.Name, r.Detail)
		}
	}
}

// KeyFileCheck checks that the control node's GitHub App private key is not
// readable by group or other, the same check control makes on start.
func KeyFileCheck(path string) Check {
	return Check{Name: "github app private key", Group: GroupOperator, Run: func(context.Context) (string, error) {
		if err := control.CheckPrivateKeyFileMode(path); err != nil {
			return "", err
		}
		return path + " is readable by its owner only", nil
	}, Fix: func() error {
		// Narrowing a key to its owner only is what control asks for and
		// keeps the bytes as they are.
		return os.Chmod(path, 0o600)
	}}
}

func doctorFlags() (fs *flag.FlagSet, natsURL, steward, natsToken, keyFile *string, asJSON, fix *bool) {
	fs = flag.NewFlagSet("schmeide doctor", flag.ContinueOnError)
	steward = fs.String("steward", "", "Your GitHub login, which the runner connects to the control node as.")
	natsURL = fs.String("nats-url", runner.DefaultNATSURL, "NATS URL of the control node, which is its --nats-addr.")
	natsToken = fs.String("nats-token", "", "This steward's NATS token: the one the control node's --runner-tokens lists for --steward.")
	keyFile = fs.String("github-app-private-key-file", "", "Path of the control node's GitHub App private key. When set, doctor fails if group or other can read it, as control refuses to start then. Skipped when empty.")
	asJSON = fs.Bool("json", false, "Write the checks to stdout as a JSON list of {name, group, status, detail}.")
	fix = fs.Bool("fix", false, "Fix failed checks that can be fixed safely, then check them again. Today only the private key's mode is fixed.")
	return fs, natsURL, steward, natsToken, keyFile, asJSON, fix
}

// Usage is the help text of `schmeide doctor`.
func Usage() string {
	fs, _, _, _, _, _, _ := doctorFlags()
	return `schmeide doctor checks that this machine can run 'schmeide runner': git is
installed, gh is installed and logged in, the disk is not full, at least one
harness is installed and logged in, and the control node is reachable. Checks
are grouped by who fixes them: the operator (logins, flags, tokens) or the
machine (installed tools, disk). A harness that fails while another passes is
reported as not selected and does not fail the check. Each failed check says
how to fix it. The exit code is 0 when every other check and at least one
harness pass, and 1 otherwise. On the control node, pass
--github-app-private-key-file to also check that the key is not readable by
group or other ('chmod 0600' fixes it, and so does --fix); without it that
check is skipped.

Usage:
  schmeide doctor [flags]

Every flag can also be set through its environment variable. A flag wins over
the environment.

` + runner.FlagUsage(fs)
}

// Main runs `schmeide doctor` with the given arguments and returns its exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs, natsURL, steward, natsToken, keyFile, asJSON, fix := doctorFlags()
	if err := runner.ParseFlags(fs, args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage())
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "schmeide doctor: %v\nRun 'schmeide doctor --help' for usage.\n", err)
		return 2
	}
	checks := Checks(*natsURL, *steward, *natsToken, harness.All())
	if *keyFile != "" {
		checks = append(checks, KeyFileCheck(*keyFile))
	}
	results, ok, fixed := Run(context.Background(), checks, *fix)
	// With --json, stdout holds only the JSON, so notes go to stderr.
	notes := stdout
	if *asJSON {
		notes = stderr
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintf(stderr, "schmeide doctor: %v\n", err)
			return 1
		}
	} else {
		writeText(stdout, results)
	}
	switch {
	case !*fix:
	case len(fixed) == 0:
		fmt.Fprintln(notes, "--fix: nothing was changed; no failed check can be fixed safely.")
	default:
		fmt.Fprintf(notes, "--fix: fixed %s.\n", strings.Join(fixed, ", "))
	}
	if !ok {
		fmt.Fprintln(stderr, "schmeide doctor: fix the failed checks above, then run 'schmeide doctor' again.")
		return 1
	}
	if *asJSON {
		return 0
	}
	fmt.Fprintln(stdout, "This machine is ready. Start the runner with 'schmeide runner --steward <your GitHub login>'.")
	return 0
}
