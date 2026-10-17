package main

import (
	"bytes"
	"testing"
)

func TestVersionUnstamped(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if out.String() != "dev\n" {
		t.Errorf("got %q, want dev", out.String())
	}
	if code := run([]string{"version", "extra"}, &out, &errb); code != 2 {
		t.Errorf("extra argument: exit %d, want 2", code)
	}
}
