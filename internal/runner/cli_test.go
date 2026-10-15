package runner

import (
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
			got := harnesses(tt.claudeArgs)
			if len(got) != len(all) {
				t.Fatalf("len = %d, harness.All() has %d", len(got), len(all))
			}
			wantArgs := strings.Fields(tt.claudeArgs)
			sawClaude := false
			for i := range all {
				if got[i].Name() != all[i].Name() {
					t.Errorf("[%d] = %s, harness.All() has %s", i, got[i].Name(), all[i].Name())
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
