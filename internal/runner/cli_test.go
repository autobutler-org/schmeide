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

func TestRunnerHelpDoesNotLimitHarnessesToClaude(t *testing.T) {
	got := Usage()
	if strings.Contains(strings.ToLower(got), "only claude code") {
		t.Fatalf("help limits the runner to Claude Code:\n%s", got)
	}
	if !strings.Contains(got, "harnesses that are installed and logged in") {
		t.Fatalf("help does not say the runner uses the harnesses installed on this machine:\n%s", got)
	}
}
