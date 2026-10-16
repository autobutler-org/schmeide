package lintgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectMakefileCheck(t *testing.T) {
	t.Run("no makefile", func(t *testing.T) {
		found, err := Detect(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if found.MakeTarget != "" || found.PreCommit != "" {
			t.Fatalf("Detect = %+v, want nothing in a directory with no Makefile", found)
		}
	})

	t.Run("check target", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "Makefile", "check: check/go\n\ttrue\n")
		found, err := Detect(dir)
		if err != nil {
			t.Fatal(err)
		}
		if found.MakeTarget != "check" {
			t.Fatalf("Detect = %+v, want Makefile target check", found)
		}
	})
}

func TestDetectIgnoresOtherMakeNames(t *testing.T) {
	for _, body := range []string{
		"check/go:\n\ttrue\n",
		"CHECK = 1\n",
		"check := 1\n",
		"# check:\n\ttrue\n",
		".PHONY: check\n",
		"check: CFLAGS = -O2\n",
		"define BODY\ncheck:\n\ttrue\nendef\n",
	} {
		dir := t.TempDir()
		writeFile(t, dir, "Makefile", body)
		found, err := Detect(dir)
		if err != nil {
			t.Fatal(err)
		}
		if found.MakeTarget != "" {
			t.Errorf("makefile %q detected %q", body, found.MakeTarget)
		}
	}
}

func TestDetectContinuedCheckTarget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Makefile", "check: \\\n\tdeps\n\ttrue\n")
	found, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if found.MakeTarget != "check" {
		t.Fatalf("Detect = %+v, want a continued check target", found)
	}
}

func TestDetectPrefersGNUmakefile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "GNUmakefile", "all:\n\ttrue\n")
	writeFile(t, dir, "Makefile", "check:\n\ttrue\n")
	found, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if found.MakeTarget != "" {
		t.Fatalf("Detect = %+v, want the GNUmakefile, which has no check target", found)
	}
}

func TestDetectPreCommitConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".pre-commit-config.yaml", "repos: []\n")
	found, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if found.PreCommit != ".pre-commit-config.yaml" || found.MakeTarget != "" {
		t.Fatalf("Detect = %+v, want the pre-commit config only", found)
	}
}

func TestWarningNamesDetectedTargetAndGate(t *testing.T) {
	const gate = "test ! -e broken"
	found := Found{MakeTarget: "check"}
	got := Warning(gate, found)
	if !strings.Contains(got, "check") || !strings.Contains(got, gate) {
		t.Fatalf("Warning = %q, want the detected target and the configured gate", got)
	}
	if !strings.Contains(got, "warning") || !strings.Contains(got, "still runs") {
		t.Fatalf("Warning = %q, want a warning that the job still runs", got)
	}
}

func TestWarningSilentWhenGateMentionsDetectedTargets(t *testing.T) {
	cases := []struct {
		gate  string
		found Found
	}{
		{gate: "make check", found: Found{MakeTarget: "check"}},
		{gate: "make check test", found: Found{MakeTarget: "check"}},
		{gate: "pre-commit run --all-files", found: Found{PreCommit: ".pre-commit-config.yaml"}},
		{gate: "make check && pre-commit run", found: Found{MakeTarget: "check", PreCommit: ".pre-commit-config.yaml"}},
		{gate: "true", found: Found{}},
	}
	for _, tc := range cases {
		if got := Warning(tc.gate, tc.found); got != "" {
			t.Errorf("Warning(%q, %+v) = %q, want empty", tc.gate, tc.found, got)
		}
	}
}

func TestWarningWhenGateMissesPreCommit(t *testing.T) {
	found := Found{MakeTarget: "check", PreCommit: ".pre-commit-config.yaml"}
	got := Warning("make check", found)
	if !strings.Contains(got, "pre-commit") || !strings.Contains(got, "make check") {
		t.Fatalf("Warning = %q, want the pre-commit config and the configured gate", got)
	}
	if strings.Contains(got, "does not mention `check`") {
		t.Fatalf("Warning = %q, want no complaint about check, which the gate names", got)
	}
}

func TestMentionsWholeWordOnly(t *testing.T) {
	if mentions("make check/go", "check") {
		t.Error("check/go was treated as the check target")
	}
	if mentions("echo checkout", "check") {
		t.Error("checkout was treated as check")
	}
	if !mentions(`sh -c "make check"`, "check") {
		t.Error(`quoted "make check" did not mention check`)
	}
}
