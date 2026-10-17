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

// TestFakeMergePullMarksItMerged closes the merged pull request and stamps
// when it merged, as the pull request list then shows it.
func TestFakeMergePullMarksItMerged(t *testing.T) {
	f := &Fake{}
	if _, err := f.OpenPull(t.Context(), "t", "b", "7_x", "main"); err != nil {
		t.Fatal(err)
	}
	if err := f.MergePull(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if p, ok, err := f.IssuePull(t.Context(), 7, "closed"); err != nil || !ok || p.MergedAt == "" {
		t.Errorf("IssuePull = %+v, %v, %v; want closed and merged", p, ok, err)
	}
}
