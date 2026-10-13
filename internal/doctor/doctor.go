// Package doctor checks that a machine is ready to run `schmeide runner` and
// says how to fix whatever is not.
package doctor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"

	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/runner"
)

// Check is one thing doctor looks at.
type Check struct {
	Name string
	// Run returns a short description of what it found, or an error whose
	// text says how to fix the problem.
	Run func(ctx context.Context) (string, error)
}

// Checks returns the checks for a runner that would use the given control
// node and harnesses.
func Checks(natsURL, natsToken string, harnesses []harness.Harness) []Check {
	checks := []Check{{Name: "git", Run: func(context.Context) (string, error) {
		path, err := exec.LookPath("git")
		if err != nil {
			return "", errors.New("git was not found on PATH; install it (https://git-scm.com/downloads) and try again")
		}
		return path, nil
	}}}
	for _, h := range harnesses {
		checks = append(checks, Check{Name: "harness " + h.Name(), Run: func(ctx context.Context) (string, error) {
			return "installed and logged in", h.Check(ctx)
		}})
	}
	return append(checks, Check{Name: "control node", Run: func(ctx context.Context) (string, error) {
		nc, _, err := runner.Connect(ctx, natsURL, natsToken)
		if err != nil {
			return "", err
		}
		nc.Close()
		return "reachable at " + natsURL, nil
	}})
}

// Report runs the checks, writes one result per check to w, and reports
// whether all of them passed.
func Report(ctx context.Context, w io.Writer, checks []Check) bool {
	ok := true
	for _, c := range checks {
		found, err := c.Run(ctx)
		if err != nil {
			ok = false
			fmt.Fprintf(w, "FAIL  %s: %v\n", c.Name, err)
			continue
		}
		fmt.Fprintf(w, "ok    %s: %s\n", c.Name, found)
	}
	return ok
}

func doctorFlags() (fs *flag.FlagSet, natsURL, natsToken *string) {
	fs = flag.NewFlagSet("schmeide doctor", flag.ContinueOnError)
	natsURL = fs.String("nats-url", runner.DefaultNATSURL, "NATS URL of the control node, which is its --nats-addr.")
	natsToken = fs.String("nats-token", "", "Token the control node's NATS expects, which is its --nats-token.")
	return fs, natsURL, natsToken
}

// Usage is the help text of `schmeide doctor`.
func Usage() string {
	fs, _, _ := doctorFlags()
	return `schmeide doctor checks that this machine can run 'schmeide runner': git is
installed, a harness is installed and logged in, and the control node is
reachable. Each failed check says how to fix it. The exit code is 0 when every
check passes and 1 otherwise.

Usage:
  schmeide doctor [flags]

Every flag can also be set through its environment variable. A flag wins over
the environment.

` + runner.FlagUsage(fs)
}

// Main runs `schmeide doctor` with the given arguments and returns its exit
// code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs, natsURL, natsToken := doctorFlags()
	if err := runner.ParseFlags(fs, args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage())
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "schmeide doctor: %v\nRun 'schmeide doctor --help' for usage.\n", err)
		return 2
	}
	if !Report(context.Background(), stdout, Checks(*natsURL, *natsToken, harness.All())) {
		fmt.Fprintln(stderr, "schmeide doctor: fix the failed checks above, then run 'schmeide doctor' again.")
		return 1
	}
	fmt.Fprintln(stdout, "This machine is ready. Start the runner with 'schmeide runner --steward <your GitHub login>'.")
	return 0
}
