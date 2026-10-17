package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/forge"
)

// headWorkspace is a Workspace whose only working method is Head.
type headWorkspace struct{ Workspace }

func (headWorkspace) Head(context.Context) (string, error) { return "abc123", nil }

// mergeServer is a GitHub API with green checks on abc123 and pull request
// 9. queue says whether its base branch has a merge queue. It records the
// calls it gets as "METHOD path" or "graphql <operation>".
func mergeServer(t *testing.T, queue bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		reply := `{}`
		switch r.URL.Path {
		case "/repos/" + testRepo + "/commits/abc123/check-runs":
			reply = `{"total_count":1,"check_runs":[{"name":"ci","status":"completed","conclusion":"success"}]}`
		case "/repos/" + testRepo + "/branches/main/protection":
			http.Error(w, `{"message":"Branch not protected"}`, http.StatusNotFound)
			return
		case "/repos/" + testRepo + "/pulls/9/merge":
			reply = `{"merged":true}`
		case "/graphql":
			var req struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("graphql body: %v", err)
			}
			switch {
			case strings.Contains(req.Query, "enqueuePullRequest"):
				call = "graphql enqueue " + req.Variables["id"].(string)
				reply = `{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"id":"MQE_1"}}}}`
			case strings.Contains(req.Query, "mergeQueue"):
				call = "graphql mergeQueue " + req.Variables["branch"].(string)
				reply = `{"data":{"repository":{"mergeQueue":null}}}`
				if queue {
					reply = `{"data":{"repository":{"mergeQueue":{"id":"MQ_1"}}}}`
				}
			case strings.Contains(req.Query, "pullRequest"):
				call = "graphql pullRequest"
				reply = `{"data":{"repository":{"pullRequest":{"id":"PR_9","baseRefName":"main"}}}}`
			}
		default:
			t.Errorf("unexpected call %s", call)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestMergeWhenGreenEnqueuesWithMergeQueue(t *testing.T) {
	srv, calls := mergeServer(t, true)
	gh := forge.GitHub{API: srv.URL, Repo: testRepo, Token: testToken}
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, "main", "https://github.com/o/r/pull/9", time.Minute, nil)
	if out.state != StateMerged || !strings.Contains(out.comment, "merge queue") {
		t.Fatalf("outcome = %+v, want merged with a merge queue comment", out)
	}
	got := strings.Join(calls(), "\n")
	if !strings.Contains(got, "graphql enqueue PR_9") || !strings.Contains(got, "graphql mergeQueue main") {
		t.Fatalf("calls = %s, want the queue read for main and PR_9 enqueued", got)
	}
	if strings.Contains(got, "/pulls/9/merge") {
		t.Fatalf("calls = %s, want no direct merge", got)
	}
}

func TestMergeWhenGreenMergesWithoutMergeQueue(t *testing.T) {
	srv, calls := mergeServer(t, false)
	gh := forge.GitHub{API: srv.URL, Repo: testRepo, Token: testToken}
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, "main", "https://github.com/o/r/pull/9", time.Minute, nil)
	if out.state != StateMerged || strings.Contains(out.comment, "merge queue") {
		t.Fatalf("outcome = %+v, want merged without a merge queue comment", out)
	}
	got := strings.Join(calls(), "\n")
	if !strings.Contains(got, "PUT /repos/"+testRepo+"/pulls/9/merge") {
		t.Fatalf("calls = %s, want the direct merge", got)
	}
	if strings.Contains(got, "graphql enqueue") {
		t.Fatalf("calls = %s, want no enqueue", got)
	}
}

func TestMergeWhenGreenRefusesRequiredReviews(t *testing.T) {
	gh := &forge.Fake{
		Checks:      []forge.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
		Protections: map[string]forge.Protection{"main": {RequiredApprovals: 1}},
	}
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, "main", "https://github.com/o/r/pull/9", time.Minute, nil)
	if out.state != StateMergeFailed {
		t.Fatalf("outcome = %+v, want merge failed", out)
	}
	for _, want := range []string{"left the pull request open", "https://github.com/o/r/pull/9", "approving review", "cannot supply"} {
		if !strings.Contains(out.comment, want) {
			t.Errorf("comment = %q, want it to name %q", out.comment, want)
		}
	}
	if len(gh.Merged()) != 0 || len(gh.Enqueued()) != 0 {
		t.Fatalf("merged %v, enqueued %v, want neither", gh.Merged(), gh.Enqueued())
	}
}

func TestMergeWhenGreenMergesAnUnprotectedBranch(t *testing.T) {
	gh := &forge.Fake{
		Checks: []forge.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
		// Protection on another branch does not apply to main.
		Protections: map[string]forge.Protection{"release": {RequiredApprovals: 2}},
	}
	out := (&Runner{}).mergeWhenGreen(t.Context(), headWorkspace{}, gh, "main", "https://github.com/o/r/pull/9", time.Minute, nil)
	if out.state != StateMerged {
		t.Fatalf("outcome = %+v, want merged", out)
	}
	if got := gh.Merged(); len(got) != 1 || got[0] != 9 {
		t.Fatalf("merged = %v, want pull 9", got)
	}
}
