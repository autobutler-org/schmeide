package main

import (
	"fmt"
	"io"

	"github.com/autobutler-org/schmeide/internal/release"
)

const versionUsage = "usage: schmeide version\n\nPrint the version stamped into this binary, or dev for an unstamped build.\n"

func versionMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, versionUsage)
		return 0
	}
	if len(args) != 0 {
		fmt.Fprintf(stderr, "schmeide version: takes no arguments\n%s", versionUsage)
		return 2
	}
	fmt.Fprintln(stdout, release.Version)
	return 0
}
