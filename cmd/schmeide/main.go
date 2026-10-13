// Command schmeide is the autobutler-org software factory.
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `schmeide is the autobutler-org software factory.

Usage:
  schmeide [-h | --help]

There are no commands yet; the design is in progress.
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "schmeide: unknown argument %q\nRun 'schmeide --help' for usage.\n", args[0])
	return 2
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
