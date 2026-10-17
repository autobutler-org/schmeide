package runner

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/harness"
)

func TestHarnessesFollowsAllAndAppliesClaudeArgs(t *testing.T) {
	tests := []struct {
		name       string
		claudeArgs string
	}{
		{name: "default acceptEdits", claudeArgs: "--permission-mode acceptEdits"},
		{name: "extra flags", claudeArgs: "--permission-mode acceptEdits --verbose"},
		{name: "empty means no extra flags", claudeArgs: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			all := harness.All()
			got := harnesses(tt.claudeArgs, "")
			if len(got) != len(all) {
				t.Fatalf("len = %d, harness.All() has %d", len(got), len(all))
			}
			wantArgs := strings.Fields(tt.claudeArgs)
			sawClaude := false
			for i := range all {
				if got[i].Name() != all[i].Name() {
					t.Errorf("[%d] = %s, harness.All() has %s", i, got[i].Name(), all[i].Name())
				}
				if g, ok := got[i].(*harness.Grok); ok {
					g.Args = nil // covered by TestHarnessesAppliesGrokArgs
				}
				cc, ok := got[i].(*harness.ClaudeCode)
				if !ok {
					continue
				}
				sawClaude = true
				if wantArgs == nil {
					if cc.Args != nil {
						t.Errorf("claude args = %#v, want nil", cc.Args)
					}
				} else if !slices.Equal(cc.Args, wantArgs) {
					t.Errorf("claude args = %#v, want %#v", cc.Args, wantArgs)
				}
				cc.Args = nil
				if base, ok := all[i].(*harness.ClaudeCode); ok {
					base.Args = nil
				}
			}
			if !sawClaude {
				t.Fatal("harness.All() has no Claude Code entry")
			}
			if !reflect.DeepEqual(got, all) {
				t.Errorf("harnesses(%q) = %#v, want harness.All() with Claude args applied", tt.claudeArgs, got)
			}
		})
	}
}

func TestPoolFlagDefaultsOff(t *testing.T) {
	help := Usage()
	if !strings.Contains(help, "whose steward has no registered runner") || !strings.Contains(help, "The default is off.") {
		t.Fatalf("help does not describe pool opt-in as off by default:\n%s", help)
	}
	fs, o, _, _ := runnerFlags()
	if o.Pool {
		t.Fatal("pool flag defaults on")
	}
	if err := ParseFlags(fs, nil); err != nil || o.Pool {
		t.Fatalf("ParseFlags() = %v, pool = %v; want off", err, o.Pool)
	}
	fs, o, _, _ = runnerFlags()
	if err := ParseFlags(fs, []string{"--pool"}); err != nil || !o.Pool {
		t.Fatalf("ParseFlags(--pool) = %v, pool = %v; want on", err, o.Pool)
	}
}

func TestRunnerHelpDoesNotLimitHarnessesToClaude(t *testing.T) {
	got := Usage()
	if strings.Contains(strings.ToLower(got), "only claude code") {
		t.Fatalf("help limits the runner to Claude Code:\n%s", got)
	}
	if !strings.Contains(got, "harnesses that are installed and logged in") {
		t.Fatalf("help does not say the runner uses the harnesses installed on this machine:\n%s", got)
	}
}

func TestUsageDescribesTheGateEnvironment(t *testing.T) {
	got := Usage()
	if !strings.Contains(got, "The gate inherits the runner process environment.") {
		t.Fatalf("Usage() does not say the gate inherits the runner process environment:\n%s", got)
	}
	if !strings.Contains(got, "A harness session sees the basic environment plus ANTHROPIC_, CLAUDE_, XAI_") {
		t.Fatalf("Usage() does not say a session keeps XAI_:\n%s", got)
	}
	if strings.Contains(got, "gate commands see only a basic environment") {
		t.Fatalf("Usage() says that gate commands see only a basic environment:\n%s", got)
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"A, B", []string{"A", " B"}},
		{"A,B,", []string{"A", "B"}},
		{",,A,,B", []string{"A", "B"}},
	}
	for _, c := range cases {
		if got := splitList(c.in); !slices.Equal(got, c.want) {
			t.Errorf("splitList(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMainExitCodes(t *testing.T) {
	t.Setenv("SCHMEIDE_STEWARD", "")
	cases := []struct {
		name      string
		args      []string
		code      int
		stdout    string
		stderrHas string
	}{
		{"help", []string{"--help"}, 0, Usage(), ""},
		{"bad flag", []string{"--no-such-flag"}, 2, "", "Run 'schmeide runner --help' for usage."},
		{"stray argument", []string{"--steward", "me", "extra"}, 2, "", `unexpected argument "extra"`},
		{"missing steward", nil, 2, "", "missing --steward"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Main(c.args, &stdout, &stderr); code != c.code {
				t.Fatalf("exit code %d, want %d; stderr: %s", code, c.code, stderr.String())
			}
			if stdout.String() != c.stdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), c.stdout)
			}
			if !strings.Contains(stderr.String(), c.stderrHas) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), c.stderrHas)
			}
		})
	}
}

func TestHarnessesAppliesGrokArgs(t *testing.T) {
	for _, h := range harnesses("", "--permission-mode acceptEdits --verbose") {
		if g, ok := h.(*harness.Grok); ok {
			if want := []string{"--permission-mode", "acceptEdits", "--verbose"}; !slices.Equal(g.Args, want) {
				t.Errorf("grok args = %#v, want %#v", g.Args, want)
			}
			return
		}
	}
	t.Fatal("harness.All() has no Grok entry")
}

func TestHarnessArgsPermissionFloor(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string // "" means accepted
	}{
		{"defaults", nil, ""},
		{"default with extra flags", []string{"--claude-args", "--permission-mode acceptEdits --verbose"}, ""},
		{"empty", []string{"--claude-args", "", "--grok-args", ""}, ""},
		{"claude skip permissions", []string{"--claude-args", "--dangerously-skip-permissions"}, `--claude-args contains "--dangerously-skip-permissions"`},
		{"claude bypassPermissions", []string{"--claude-args", "--permission-mode bypassPermissions"}, `--claude-args contains "--permission-mode bypassPermissions"`},
		{"claude mode with equals", []string{"--claude-args", "--permission-mode=plan"}, `--claude-args contains "--permission-mode=plan"`},
		{"grok skip permissions", []string{"--grok-args", "--permission-mode acceptEdits --dangerously-skip-permissions"}, `--grok-args contains "--dangerously-skip-permissions"`},
		{"grok bypassPermissions", []string{"--grok-args", "--permission-mode bypassPermissions"}, `--grok-args contains "--permission-mode bypassPermissions"`},
		{"opt-in claude", []string{"--allow-dangerous-harness-args", "--claude-args", "--dangerously-skip-permissions"}, ""},
		{"opt-in grok", []string{"--allow-dangerous-harness-args", "--grok-args", "--permission-mode bypassPermissions"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, _, _, h := runnerFlags()
			if err := ParseFlags(fs, c.args); err != nil {
				t.Fatal(err)
			}
			err := h.check()
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("check() = %v, want nil", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("check() = nil, want an error containing %q", c.wantErr)
			case c.wantErr != "" && (!strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "--allow-dangerous-harness-args")):
				t.Fatalf("check() = %v, want it to name %q and --allow-dangerous-harness-args", err, c.wantErr)
			}
		})
	}
}

func TestMainRefusesWideningHarnessArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--steward", "me", "--claude-args", "--dangerously-skip-permissions"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code %d, want 2; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{"--dangerously-skip-permissions", "--allow-dangerous-harness-args"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not name %s", stderr.String(), want)
		}
	}
}

func TestUsageStatesPermissionFloor(t *testing.T) {
	got := Usage()
	for _, want := range []string{"a session may edit", "not handed flags that skip permission", "--allow-dangerous-harness-args", "docs/permissions.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("Usage() does not contain %q", want)
		}
	}
}
