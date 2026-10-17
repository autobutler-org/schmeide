package forge

import (
	"testing"
	"time"
)

// TestFakeCommentsSince leaves out a comment stored before since and keeps
// one stored at or after it.
func TestFakeCommentsSince(t *testing.T) {
	f := &Fake{}
	if err := f.Comment(t.Context(), 7, "old"); err != nil {
		t.Fatal(err)
	}
	// The sleeps keep a coarse clock from stamping both sides of since alike.
	time.Sleep(time.Millisecond)
	since := time.Now().Format(time.RFC3339Nano)
	time.Sleep(time.Millisecond)
	if err := f.Comment(t.Context(), 7, "new"); err != nil {
		t.Fatal(err)
	}

	got, err := f.Comments(t.Context(), 7, since)
	if err != nil || len(got) != 1 || got[0].Body != "new" || got[0].User.Type != "Bot" {
		t.Errorf("Comments since = %+v, %v; want only new", got, err)
	}
	if got, err := f.Comments(t.Context(), 7, ""); err != nil || len(got) != 2 {
		t.Errorf("Comments = %+v, %v; want both", got, err)
	}
	if _, err := f.Comments(t.Context(), 7, "yesterday"); err == nil {
		t.Error("Comments since yesterday: no error, want one")
	}
}
