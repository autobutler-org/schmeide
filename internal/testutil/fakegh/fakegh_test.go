package fakegh

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const (
	repo  = "o/r"
	token = "tok"
)

// call makes one request and decodes the JSON reply.
func call(t *testing.T, s *Server, method, path, body string) (int, any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, s.URL+"/repos/"+repo+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s %s: body %q is not JSON: %v", method, path, data, err)
	}
	return resp.StatusCode, out
}

func hasKeys(t *testing.T, what string, v any, keys ...string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want an object", what, v)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s has no %q: %v", what, k, m)
		}
	}
	return m
}

func list(t *testing.T, what string, v any, n int) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok || len(l) != n {
		t.Fatalf("%s = %v, want %d items", what, v, n)
	}
	return l
}

func TestContract(t *testing.T) {
	s := New(t, repo, token)

	code, got := call(t, s, http.MethodGet, "", "")
	if code != http.StatusOK || hasKeys(t, "repo", got, "default_branch")["default_branch"] != "main" {
		t.Errorf("GET repo = %d %v", code, got)
	}

	code, got = call(t, s, http.MethodPost, "/pulls", `{"title":"t","body":"b","head":"7_x","base":"main"}`)
	if code != http.StatusCreated {
		t.Errorf("POST pulls = %d, want 201", code)
	}
	pr := hasKeys(t, "pull", got, "number", "html_url", "state", "created_at", "head", "base")
	hasKeys(t, "pull head", pr["head"], "ref")
	if pr["number"] != 1.0 || pr["state"] != "open" {
		t.Errorf("pull = %v, want open #1", pr)
	}
	if code, _ := call(t, s, http.MethodPost, "/pulls", `{"title":"t"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("POST pulls without head = %d, want 422", code)
	}

	s.SeedPull(Pull{Title: "old", Head: "3_y", Base: "main", State: "closed"})
	for q, want := range map[string]int{
		"":                             1,
		"?state=open":                  1,
		"?state=closed":                1,
		"?state=all":                   2,
		"?state=all&head=o:7_x":        1,
		"?state=all&head=o:nope":       0,
		"?state=all&per_page=1":        1,
		"?state=all&per_page=1&page=2": 1,
		"?state=all&per_page=1&page=3": 0,
	} {
		code, got := call(t, s, http.MethodGet, "/pulls"+q, "")
		if code != http.StatusOK {
			t.Errorf("GET pulls%s = %d", q, code)
		}
		list(t, "pulls"+q, got, want)
	}

	if code, _ := call(t, s, http.MethodPost, "/issues/7/comments", `{"body":"hi"}`); code != http.StatusCreated {
		t.Errorf("POST comment = %d, want 201", code)
	}
	code, got = call(t, s, http.MethodGet, "/issues/7/comments?per_page=100&page=1", "")
	c := hasKeys(t, "comment", list(t, "comments", got, 1)[0], "id", "body", "user")
	if code != http.StatusOK || hasKeys(t, "comment user", c["user"], "type")["type"] != "Bot" {
		t.Errorf("GET comments = %d %v", code, got)
	}

	s.AddBlocker(7, Issue{Number: 3, State: "open"})
	code, got = call(t, s, http.MethodGet, "/issues/7/dependencies/blocked_by?per_page=100&page=1", "")
	hasKeys(t, "blocker", list(t, "blocked_by", got, 1)[0], "number", "state")
	if code != http.StatusOK {
		t.Errorf("GET blocked_by = %d", code)
	}
	_, got = call(t, s, http.MethodGet, "/issues/8/dependencies/blocked_by", "")
	list(t, "no blockers", got, 0)

	s.SetChecks([]CheckRun{{Name: "a", Status: "completed", Conclusion: "success"}, {Name: "b", Status: "queued"}})
	code, got = call(t, s, http.MethodGet, "/commits/abc/check-runs?filter=latest&per_page=1&page=2", "")
	runs := hasKeys(t, "check runs", got, "total_count", "check_runs")
	run := hasKeys(t, "check run", list(t, "check_runs", runs["check_runs"], 1)[0], "name", "status", "conclusion")
	if code != http.StatusOK || runs["total_count"] != 2.0 || run["name"] != "b" {
		t.Errorf("GET check-runs page 2 = %d %v", code, runs)
	}

	if code, _ := call(t, s, http.MethodGet, "/branches/main/protection", ""); code != http.StatusNotFound {
		t.Errorf("GET unprotected branch = %d, want 404", code)
	}
	s.Protect("release/1", 2)
	code, got = call(t, s, http.MethodGet, "/branches/release/1/protection", "")
	reviews := hasKeys(t, "protection", got, "required_pull_request_reviews")["required_pull_request_reviews"]
	if code != http.StatusOK || hasKeys(t, "reviews", reviews, "required_approving_review_count")["required_approving_review_count"] != 2.0 {
		t.Errorf("GET protection = %d %v", code, got)
	}

	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			s.FailMerge(status)
			code, got := call(t, s, http.MethodPut, "/pulls/1/merge", `{"merge_method":"merge"}`)
			if code != status || hasKeys(t, "merge error", got, "message", "documentation_url")["message"] != mergeErrors[status] {
				t.Errorf("PUT merge = %d %v, want %d", code, got, status)
			}
		})
	}
	if n, _ := s.Merges(); n != 0 {
		t.Errorf("merges = %d after failures, want 0", n)
	}
	s.mu.Lock()
	s.mergeStatus = 0
	s.mu.Unlock()

	code, got = call(t, s, http.MethodPut, "/pulls/1/merge", `{"merge_method":"merge"}`)
	if m := hasKeys(t, "merge", got, "sha", "merged", "message"); code != http.StatusOK || m["merged"] != true {
		t.Errorf("PUT merge = %d %v", code, got)
	}
	if n, method := s.Merges(); n != 1 || method != "merge" {
		t.Errorf("merges = %d %q", n, method)
	}
	if code, _ := call(t, s, http.MethodPut, "/pulls/1/merge", `{"merge_method":"merge"}`); code != http.StatusMethodNotAllowed {
		t.Errorf("merging a merged pull = %d, want 405", code)
	}
	if code, _ := call(t, s, http.MethodPut, "/pulls/99/merge", `{}`); code != http.StatusNotFound {
		t.Errorf("merging a missing pull = %d, want 404", code)
	}
	if pulls, _ := s.Snapshot(); pulls[0].State != "closed" || !pulls[0].Merged {
		t.Errorf("merged pull = %+v, want closed and merged", pulls[0])
	}
}

func TestJobLogs(t *testing.T) {
	s := New(t, repo, token)
	s.SetLog(42, "step 1\nFAIL\n")
	get := func(id string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/repos/"+repo+"/actions/jobs/"+id+"/logs", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(data)
	}
	if code, body := get("42"); code != http.StatusOK || body != "step 1\nFAIL\n" {
		t.Errorf("GET job 42 logs = %d %q, want the log", code, body)
	}
	if code, _ := get("43"); code != http.StatusNotFound {
		t.Errorf("GET job 43 logs = %d, want 404 for a job without a log", code)
	}
}
