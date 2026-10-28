package ciwatch

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNeedsTail(t *testing.T) {
	red := Run{ID: 7, Name: "lint", Status: "completed", Conclusion: "failure"}
	green := Run{ID: 6, Name: "test", Status: "completed", Conclusion: "success"}
	queued := Run{ID: 8, Name: "build", Status: "queued"}
	for _, tt := range []struct {
		name  string
		runs  []Run
		fixed bool
		want  Run
		ok    bool
	}{
		{"first failure", []Run{green, queued, red}, false, red, true},
		{"after the fix round", []Run{green, red}, true, Run{}, false},
		{"green and pending", []Run{green, queued}, false, Run{}, false},
		{"skipped is not a failure", []Run{{Name: "build-android", Status: "completed", Conclusion: "skipped"}, red}, false, red, true},
		{"only skipped", []Run{{Name: "build-android", Status: "completed", Conclusion: "skipped"}}, false, Run{}, false},
		{"no runs", nil, false, Run{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NeedsTail(tt.runs, tt.fixed)
			if got != tt.want || ok != tt.ok {
				t.Errorf("NeedsTail = %+v, %v, want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTailCapsTheLog(t *testing.T) {
	if got := Tail([]byte("short log")); got != "short log" {
		t.Errorf("Tail = %q, want the whole short log", got)
	}
	log := "HEAD\n" + strings.Repeat("x", 2*TailBytes) + "\nFAIL: TestThing"
	got := Tail([]byte(log))
	if len(got) != TailBytes || !strings.HasSuffix(got, "FAIL: TestThing") || strings.Contains(got, "HEAD") {
		t.Errorf("Tail is %d bytes ending %q, want the last %d bytes", len(got), got[len(got)-20:], TailBytes)
	}
}

func TestTailStartsOnACharacter(t *testing.T) {
	// "é" is two bytes, so the cut lands inside one.
	got := Tail([]byte(strings.Repeat("é", TailBytes) + "a"))
	if !utf8.ValidString(got) || len(got) != TailBytes-1 {
		t.Errorf("Tail is %d bytes, valid %v, want %d valid bytes", len(got), utf8.ValidString(got), TailBytes-1)
	}
}
