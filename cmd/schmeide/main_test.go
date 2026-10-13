package main

import (
	"bytes"
	"strings"
	"testing"
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
