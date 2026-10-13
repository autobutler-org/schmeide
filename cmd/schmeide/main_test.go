package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/doctor"
	"github.com/autobutler-org/schmeide/internal/runner"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no args prints usage", wantStdout: usage},
		{name: "short help", args: []string{"-h"}, wantStdout: usage},
		{name: "long help", args: []string{"--help"}, wantStdout: usage},
		{name: "unknown flag", args: []string{"--nope"}, wantCode: 2, wantStderr: "schmeide --help"},
		{name: "unknown command", args: []string{"nope"}, wantCode: 2, wantStderr: "schmeide --help"},
	}
	t.Setenv("SCHMEIDE_WEBHOOK_SECRET", "")
	t.Setenv("SCHMEIDE_GITHUB_APP_ID", "")
	t.Setenv("SCHMEIDE_STEWARD", "")
	t.Setenv("SCHMEIDE_NATS_TOKEN", "")
	const noControl = "nats://127.0.0.1:1" // nothing listens on port 1
	tests = append(tests, []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "control help", args: []string{"control", "--help"}, wantStdout: control.Usage()},
		{name: "control unknown flag", args: []string{"control", "--nope"}, wantCode: 2, wantStderr: "schmeide control --help"},
		{name: "control missing settings names flag", args: []string{"control"}, wantCode: 2, wantStderr: "pass --webhook-secret"},
		{name: "control missing settings names env", args: []string{"control"}, wantCode: 2, wantStderr: "set SCHMEIDE_GITHUB_APP_ID"},
		{name: "runner help", args: []string{"runner", "--help"}, wantStdout: runner.Usage()},
		{name: "runner unknown flag", args: []string{"runner", "--nope"}, wantCode: 2, wantStderr: "schmeide runner --help"},
		{name: "runner missing steward", args: []string{"runner"}, wantCode: 2, wantStderr: "pass your GitHub login with --steward"},
		{
			name: "runner without a control node says what to check", args: []string{"runner", "--steward", "alice", "--nats-url", noControl},
			wantCode: 1, wantStderr: "Check that `schmeide control` is running",
		},
		{name: "doctor help", args: []string{"doctor", "--help"}, wantStdout: doctor.Usage()},
		{name: "doctor unknown flag", args: []string{"doctor", "--nope"}, wantCode: 2, wantStderr: "schmeide doctor --help"},
	}...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tt.args, &stdout, &stderr); got != tt.wantCode {
				t.Errorf("exit code = %d, want %d", got, tt.wantCode)
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
