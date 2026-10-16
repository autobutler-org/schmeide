package runner

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/control"
)

// TestMain keeps trust decisions out of the real config directory and trusts
// every pair, so the job tests in this package run unattended. A test about
// trust clears SCHMEIDE_TRUST itself.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "schmeide-trust-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv("SCHMEIDE_TRUST", "1")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestWorkRefusesAnUntrustedPair(t *testing.T) {
	t.Setenv("SCHMEIDE_TRUST", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// No GitHub API is set up: the refusal must come before any call to it.
	r := &Runner{opts: Options{WorkDir: t.TempDir(), GitHubURL: "https://github.invalid"}}
	out := r.work(t.Context(), github{}, control.Job{Repo: "acme/widgets", Issue: 1, Title: "x"}, "token")
	if out.state != StateFailed || !strings.Contains(out.detail, "SCHMEIDE_TRUST=1") {
		t.Fatalf("work = %+v, want a failure naming SCHMEIDE_TRUST=1", out)
	}
}

func TestConfirmReadsYes(t *testing.T) {
	for answer, want := range map[string]bool{"y\n": true, "YES\n": true, "n\n": false, "\n": false, "yes": true} {
		got, err := confirm(bufio.NewReader(strings.NewReader(answer)), io.Discard)("trust?")
		if err != nil || got != want {
			t.Errorf("confirm(%q) = %v, %v, want %v", answer, got, err, want)
		}
	}
	if _, err := confirm(bufio.NewReader(strings.NewReader("")), io.Discard)("trust?"); err == nil {
		t.Error("confirm on a closed input answered")
	}
}

func TestUsageNamesTrust(t *testing.T) {
	if !strings.Contains(Usage(), "SCHMEIDE_TRUST=1") {
		t.Error("runner help does not mention SCHMEIDE_TRUST")
	}
}
