// Package release holds the version stamped into the schmeide binary.
package release

import "golang.org/x/mod/semver"

// Version is set at build time with
// -ldflags "-X github.com/autobutler-org/schmeide/internal/release.Version=v1.2.3".
// An unstamped build reports dev.
var Version = "dev"

// Skewed reports whether a and b are both semantic versions and differ.
// A version that does not parse, such as dev, is never skewed.
func Skewed(a, b string) bool {
	return semver.IsValid(a) && semver.IsValid(b) && semver.Compare(a, b) != 0
}
