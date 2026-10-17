// Command schmeide is the autobutler-org software factory.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/doctor"
	"github.com/autobutler-org/schmeide/internal/runner"
)

const usage = `schmeide is the autobutler-org software factory.

Usage:
  schmeide <command> [flags]
  schmeide [-h | --help]

Commands:
  control     Run the control node: embedded NATS job queue and GitHub webhook intake.
  runner      Run a runner: take your jobs from the control node and open a PR for each.
  doctor      Check that this machine is ready to run a runner.
  status      Show registered runners and queued or running jobs.
  init        Write a repo config and a control node config in this directory.
  completion  Print a shell completion script for bash, zsh, or fish.
  version     Print the version stamped into this binary.

Run 'schmeide <command> --help' for a command's flags.
See docs/GLOSSARY.md for what the terms mean.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "control":
		return control.Main(args[1:], stdout, stderr)
	case "runner":
		return runner.Main(args[1:], stdout, stderr)
	case "doctor":
		return doctor.Main(args[1:], stdout, stderr)
	case "status":
		return control.StatusMain(args[1:], stdout, stderr)
	case "init":
		return initMain(args[1:], stdout, stderr)
	case "completion":
		return completionMain(args[1:], stdout, stderr)
	case "version":
		return versionMain(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "schmeide: unknown argument %q\nRun 'schmeide --help' for usage.\n", args[0])
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
