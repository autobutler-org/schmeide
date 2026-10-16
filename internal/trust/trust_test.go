package trust

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const origin = "https://github.com/acme/widgets.git"

func setup(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvVar, "")
	return t.TempDir()
}

func yes(string) (bool, error) { return true, nil }

func TestFreshPairIsRefused(t *testing.T) {
	dir := setup(t)
	err := Check(dir, origin, nil)
	if err == nil || !strings.Contains(err.Error(), "SCHMEIDE_TRUST=1") {
		t.Fatalf("Check = %v, want a refusal naming SCHMEIDE_TRUST=1", err)
	}
}

func TestEnvAllowsWithoutRecording(t *testing.T) {
	dir := setup(t)
	t.Setenv(EnvVar, "1")
	if err := Check(dir, origin, nil); err != nil {
		t.Fatalf("Check with %s=1 = %v", EnvVar, err)
	}
	t.Setenv(EnvVar, "")
	if err := Check(dir, origin, nil); err == nil {
		t.Fatal("the env var recorded a decision")
	}
}

func TestRecordedYesAllows(t *testing.T) {
	dir := setup(t)
	if err := Check(dir, origin, yes); err != nil {
		t.Fatalf("Check with a yes = %v", err)
	}
	if err := Check(dir, origin, nil); err != nil {
		t.Fatalf("Check after a recorded yes = %v", err)
	}
	// The same directory through a symlink is the same pair.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := Check(link, origin, nil); err != nil {
		t.Fatalf("Check through a symlink = %v", err)
	}
}

func TestNoIsNotRecorded(t *testing.T) {
	dir := setup(t)
	no := func(string) (bool, error) { return false, nil }
	if err := Check(dir, origin, no); err == nil {
		t.Fatal("Check with a no allowed the pair")
	}
	if err := Check(dir, origin, nil); err == nil {
		t.Fatal("a no was recorded as a yes")
	}
	boom := errors.New("closed stdin")
	if err := Check(dir, origin, func(string) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Fatalf("Check = %v, want %v", err, boom)
	}
}

func TestChangedOriginIsRefused(t *testing.T) {
	dir := setup(t)
	if err := Check(dir, origin, yes); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir, "https://github.com/acme/other.git", nil); err == nil {
		t.Fatal("a different origin in a trusted directory was allowed")
	}
}
