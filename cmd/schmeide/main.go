// Command schmeide is the autobutler-org software factory.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/autobutler-org/schmeide/internal/control"
)

const usage = `schmeide is the autobutler-org software factory.

Usage:
  schmeide <command> [flags]
  schmeide [-h | --help]

Commands:
  control   Run the control node: embedded NATS job queue and GitHub webhook intake.

Run 'schmeide <command> --help' for a command's flags.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if args[0] == "control" {
		return control.Main(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "schmeide: unknown argument %q\nRun 'schmeide --help' for usage.\n", args[0])
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
