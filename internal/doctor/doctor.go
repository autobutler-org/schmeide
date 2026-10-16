// Package doctor checks that a machine is ready to run `schmeide runner` and
// says how to fix whatever is not. One working harness is enough. A harness
// the runner will not use is reported as not selected.
package doctor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/runner"
)

// Check is one thing doctor looks at.
type Check struct {
	Name string
	// Harness is true when this check is one harness. Those checks pass as
	// a group when at least one of them passes.
	Harness bool
	// Run returns a short description of what it found, or an error whose
	// text says how to fix the problem.
	Run func(ctx context.Context) (string, error)
}

// Checks returns the checks for a runner that would use the given control
// node and harnesses.
func Checks(natsURL, steward, natsToken string, harnesses []harness.Harness) []Check {
	checks := []Check{{Name: "git", Run: func(context.Context) (string, error) {
		path, err := exec.LookPath("git")
		if err != nil {
			return "", errors.New("git was not found on PATH; install it (https://git-scm.com/downloads) and try again")
		}
		return path, nil
	}}}
	for _, h := range harnesses {
		checks = append(checks, Check{Name: "harness " + h.Name(), Harness: true, Run: func(ctx context.Context) (string, error) {
			return "installed and logged in", h.Check(ctx)
		}})
	}
	return append(checks, Check{Name: "control node", Run: func(ctx context.Context) (string, error) {
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

// Report runs the checks, writes one result per check to w, and reports
// whether this machine is ready. Git and the control node fail the report
// on their own. Harness checks pass when at least one of them passes; each
// other harness is written as not selected, with the fix its error carries,
// and that line does not fail the report. When every harness fails, each
// one is reported with its fix and the report fails.
func Report(ctx context.Context, w io.Writer, checks []Check) bool {
	type result struct {
		check Check
		found string
		err   error
	}
	// Run every check before printing. A harness failure is not selected
	// only once some other harness has passed, which may come later.
	results := make([]result, 0, len(checks))
	harnessPassed := false
	for _, c := range checks {
		found, err := c.Run(ctx)
		results = append(results, result{check: c, found: found, err: err})
		if c.Harness && err == nil {
			harnessPassed = true
		}
	}
	ok := true
	for _, r := range results {
		if r.err != nil && r.check.Harness && harnessPassed {
			fmt.Fprintf(w, "skip  %s: not selected: %v\n", r.check.Name, r.err)
			continue
		}
		if r.err != nil {
			ok = false
			fmt.Fprintf(w, "FAIL  %s: %v\n", r.check.Name, r.err)
			continue
		}
		fmt.Fprintf(w, "ok    %s: %s\n", r.check.Name, r.found)
	}
	return ok
}

// KeyFileCheck checks that the control node's GitHub App private key is not
// readable by group or other, the same check control makes on start.
func KeyFileCheck(path string) Check {
	return Check{Name: "github app private key", Run: func(context.Context) (string, error) {
		if err := control.CheckPrivateKeyFileMode(path); err != nil {
			return "", err
		}
		return path + " is readable by its owner only", nil
	}}
}

func doctorFlags() (fs *flag.FlagSet, natsURL, steward, natsToken, keyFile *string) {
	fs = flag.NewFlagSet("schmeide doctor", flag.ContinueOnError)
	steward = fs.String("steward", "", "Your GitHub login, which the runner connects to the control node as.")
	natsURL = fs.String("nats-url", runner.DefaultNATSURL, "NATS URL of the control node, which is its --nats-addr.")
	natsToken = fs.String("nats-token", "", "This steward's NATS token: the one the control node's --runner-tokens lists for --steward.")
	keyFile = fs.String("github-app-private-key-file", "", "Path of the control node's GitHub App private key. When set, doctor fails if group or other can read it, as control refuses to start then. Skipped when empty.")
	return fs, natsURL, steward, natsToken, keyFile
}

// Usage is the help text of `schmeide doctor`.
func Usage() string {
	fs, _, _, _, _ := doctorFlags()
	return `schmeide doctor checks that this machine can run 'schmeide runner': git is
installed, at least one harness is installed and logged in, and the control
node is reachable. A harness that fails while another passes is reported as
not selected and does not fail the check. Each failed check says how to fix
it. The exit code is 0 when git, the control node, and at least one harness
pass, and 1 otherwise. On the control node, pass --github-app-private-key-file
to also check that the key is not readable by group or other ('chmod 0600'
fixes it); without it that check is skipped.

Usage:
  schmeide doctor [flags]

Every flag can also be set through its environment variable. A flag wins over
the environment.

` + runner.FlagUsage(fs)
}

// Main runs `schmeide doctor` with the given arguments and returns its exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs, natsURL, steward, natsToken, keyFile := doctorFlags()
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
	if !Report(context.Background(), stdout, checks) {
		fmt.Fprintln(stderr, "schmeide doctor: fix the failed checks above, then run 'schmeide doctor' again.")
		return 1
	}
	fmt.Fprintln(stdout, "This machine is ready. Start the runner with 'schmeide runner --steward <your GitHub login>'.")
	return 0
}
