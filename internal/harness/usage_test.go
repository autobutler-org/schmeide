package harness

import (
	"errors"
	"strings"
	"testing"
)

func TestUsageLimited(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"claude legacy epoch form", "Claude AI usage limit reached|1700000000", true},
		{"claude weekly limit", "You've hit your weekly limit · resets Nov 4, 9am", true},
		{"claude hit your limit", "You've hit your limit · resets 3pm (America/New_York)", true},
		{"claude stream-json result", `{"type":"result","subtype":"success","is_error":true,"result":"Usage limit reached"}`, true},
		{"claude stream-json assistant error", `{"type":"assistant","error":"rate_limit","message":{}}`, true},
		{"claude api rate limit error", `API Error: 429 {"type":"error","error":{"type":"rate_limit_error"}}`, true},
		{"claude out of credits", "Out of usage credits", true},
		{"grok plan rate limit", "Rate limited: You've hit the rate limit for your plan.", true},
		{"grok weekly limit", "You hit your weekly limit.", true},
		{"grok credits", "out of credits or over your spending limit. Add credits and retry.", true},
		{"http 429", "request error - Too Many Requests (429)", true},
		{"case is ignored", "USAGE LIMIT REACHED", true},
		{"ordinary failure", "Error: could not find package main", false},
		{"empty", "", false},
		{"a limit unrelated to usage", "Context limit reached", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usageLimited(tt.output); got != tt.want {
				t.Errorf("usageLimited(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestUsageLimitErrorKeepsTailOutOfItsText(t *testing.T) {
	err := error(&UsageLimitError{Harness: "claude-code", Tail: "secret output"})
	if !errors.Is(err, ErrUsageLimit) {
		t.Errorf("errors.Is(%v, ErrUsageLimit) = false", err)
	}
	if strings.Contains(err.Error(), "secret output") {
		t.Errorf("Error() = %q, want no session output in it", err)
	}
}

func TestTailWriterKeepsTheEnd(t *testing.T) {
	var w tailWriter
	for range 3 * usageTailBytes {
		_, _ = w.Write([]byte("a"))
	}
	_, _ = w.Write([]byte("END"))
	got := w.String()
	if len(got) > usageTailBytes || !strings.HasSuffix(got, "END") {
		t.Errorf("tail is %d bytes ending %q, want at most %d ending END", len(got), got[len(got)-3:], usageTailBytes)
	}
}
