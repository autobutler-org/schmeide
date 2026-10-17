package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/nats-io/nats.go/jetstream"
)

// TestResumeAtReviewFromTheStageRecord stops a runner once its pull request
// is open. GitHub then leaves that pull request out of its list, as it can
// right after one opens, so only the stage record in JetStream tells the
// next runner that the job is at its review.
func TestResumeAtReviewFromTheStageRecord(t *testing.T) {
	w := newWorld(t)
	var hidePulls atomic.Bool
	lagging := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if hidePulls.Load() && req.Method == http.MethodGet && req.URL.Path == "/repos/"+testRepo+"/pulls" {
			rw.Header().Set("Content-Type", "application/json")
			_, _ = rw.Write([]byte("[]"))
			return
		}
		w.gh.ServeHTTP(rw, req)
	}))
	t.Cleanup(lagging.Close)
	w.apiURL = lagging.URL

	var first *Runner
	stopped := make(chan struct{})
	stop := sync.OnceFunc(func() {
		first.nc.Close()
		close(stopped)
	})
	h1, _ := writes(map[string]string{"fix.txt": "x"})
	work := h1.RunFunc
	h1.RunFunc = func(ctx context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			stop()
			<-ctx.Done()
			return ctx.Err()
		}
		return work(ctx, req)
	}
	first, _ = w.start(t, "alice", h1)
	w.publish(t, "alice", 7)
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("the first runner never reached its review")
	}
	for len(w.statuses) > 0 {
		<-w.statuses // what the first runner reported before it stopped
	}

	js, err := jetstream.New(w.client)
	if err != nil {
		t.Fatal(err)
	}
	stages, err := js.KeyValue(t.Context(), control.StageBucket)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimPrefix(stewardSubject("alice", 7), jobSubjectPrefix)
	entry, err := stages.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("no stage recorded for %s: %v", key, err)
	}
	var rec stage
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Stage != stageReview || rec.Round != 1 || rec.PR == "" || rec.Branch != "7_fix-the-thing" || rec.Base != "main" {
		t.Errorf("stage record = %+v, want review round 1 of the open pull request", rec)
	}

	hidePulls.Store(true)
	var workers, reviews atomic.Int32
	h2 := &harness.Fake{RunFunc: func(_ context.Context, req harness.Request) error {
		if strings.Contains(req.Prompt, reviewLead) {
			reviews.Add(1)
			return writeVerdict(req, "pass\n")
		}
		workers.Add(1)
		return nil
	}}
	w.start(t, "alice", h2)
	states := w.await(t, "alice", 7)
	if got, want := states[len(states)-1], StatePROpened; got != want || !slices.Contains(states, StateReviewing) {
		t.Errorf("second runner's states = %v, want a review that ends %s", states, want)
	}
	if workers.Load() != 0 || reviews.Load() != 1 {
		t.Errorf("workers = %d, reviews = %d, want one review and no work session", workers.Load(), reviews.Load())
	}
	pulls, comments := w.gh.Snapshot()
	if len(pulls) != 1 {
		t.Errorf("pull requests = %+v, want only the first", pulls)
	}
	if len(comments["7"]) != 1 || !strings.Contains(comments["7"][0], "review passed") {
		t.Errorf("issue comments = %q, want one outcome comment", comments["7"])
	}
	if _, err := stages.Get(t.Context(), key); err == nil {
		t.Error("the finished job's stage record was not cleared")
	}
}
