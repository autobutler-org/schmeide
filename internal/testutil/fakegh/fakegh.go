// Package fakegh is a stateful fake of the GitHub REST endpoints the runner
// calls for one repository: the repository, pulls, issue comments, check
// runs and their Actions job logs, blocked-by dependencies, and merge. List
// endpoints honor per_page and page. Merge can be told to fail the way
// GitHub does.
package fakegh

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pull is a pull request as the fake stores it.
type Pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	URL    string `json:"html_url"`
	// State is "open" or "closed". Empty is open.
	State  string `json:"state,omitempty"`
	Merged bool   `json:"merged,omitempty"`
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	// ID is also the Actions job ID whose log SetLog sets.
	ID         int64  `json:"id,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Issue is an issue as the blocked-by endpoint lists it.
type Issue struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// mergeErrors are GitHub's messages for the merge failures it documents.
var mergeErrors = map[int]string{
	http.StatusMethodNotAllowed:    "Pull Request is not mergeable",
	http.StatusConflict:            "Head branch was modified. Review and try the merge again.",
	http.StatusUnprocessableEntity: "Validation Failed",
}

const mergeDocs = "https://docs.github.com/rest/pulls/pulls#merge-a-pull-request"

// Server is the fake. Start one with New.
type Server struct {
	// URL is the API base URL to hand to the client.
	URL string

	t           testing.TB
	owner, repo string
	token       string

	mu       sync.Mutex
	onPR     func()
	pulls    []Pull
	comments map[string][]string // by issue number
	calls    []string
	checks   []CheckRun
	logs     map[int64]string // by job ID
	// merges is how many merge calls landed, and mergeMethod is the last one.
	merges      int
	mergeMethod string
	// mergeStatus, when set, is the error status every merge call returns.
	mergeStatus int
	blockedBy   map[int][]Issue
}

// New starts a fake for repo ("owner/name") that accepts only token. It is
// closed when the test ends.
func New(t testing.TB, repo, token string) *Server {
	owner, _, _ := strings.Cut(repo, "/")
	s := &Server{t: t, owner: owner, repo: repo, token: token, comments: map[string][]string{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, docs string) {
	writeJSON(w, status, map[string]string{"message": message, "documentation_url": docs})
}

// page returns the slice of items that per_page and page select. GitHub's
// defaults are 30 per page and page 1.
func page[T any](r *http.Request, items []T) []T {
	per, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || per < 1 {
		per = 30
	}
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		n = 1
	}
	start := min((n-1)*per, len(items))
	return append([]T{}, items[start:min(start+per, len(items))]...)
}

func (p Pull) reply() map[string]any {
	state := p.State
	if state == "" {
		state = "open"
	}
	return map[string]any{
		"number":     p.Number,
		"title":      p.Title,
		"body":       p.Body,
		"html_url":   p.URL,
		"state":      state,
		"merged":     p.Merged,
		"created_at": time.Date(2026, 1, 1, 0, 0, p.Number, 0, time.UTC).Format(time.RFC3339),
		"head":       map[string]string{"ref": p.Head},
		"base":       map[string]string{"ref": p.Base},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+s.token {
		s.t.Errorf("%s %s: Authorization = %q, want the job's token", r.Method, r.URL, got)
		writeError(w, http.StatusUnauthorized, "Bad credentials", "https://docs.github.com/rest")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
		// ponytail: every GraphQL query gets one answer, a pull request on
		// main whose branch has no merge queue.
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequest": map[string]string{"id": "PR_1", "baseRefName": "main"},
			"mergeQueue":  nil,
		}}})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/"+s.repo)
	if !ok {
		s.unexpected(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "":
		writeJSON(w, http.StatusOK, map[string]any{"full_name": s.repo, "default_branch": "main"})
	case r.Method == http.MethodGet && path == "/pulls":
		s.listPulls(w, r)
	case r.Method == http.MethodPost && path == "/pulls":
		s.createPull(w, r)
	case r.Method == http.MethodPut && len(parts) == 3 && parts[0] == "pulls" && parts[2] == "merge":
		s.merge(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "issues" && parts[2] == "comments":
		if _, err := strconv.Atoi(parts[1]); err != nil {
			s.unexpected(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			// ponytail: since is ignored; every comment counts as new.
			found := []map[string]any{}
			for i, body := range s.comments[parts[1]] {
				// Every comment here is the app's own, so each is a bot's.
				found = append(found, map[string]any{"id": i + 1, "body": body, "user": map[string]string{"type": "Bot"}})
			}
			writeJSON(w, http.StatusOK, page(r, found))
		case http.MethodPost:
			var c struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil || c.Body == "" {
				writeError(w, http.StatusUnprocessableEntity, "Validation Failed", "https://docs.github.com/rest/issues/comments#create-an-issue-comment")
				return
			}
			s.comments[parts[1]] = append(s.comments[parts[1]], c.Body)
			writeJSON(w, http.StatusCreated, map[string]any{"id": len(s.comments[parts[1]]), "body": c.Body, "user": map[string]string{"type": "Bot"}})
		default:
			s.unexpected(w, r)
		}
	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "issues" && parts[2] == "dependencies" && parts[3] == "blocked_by":
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			s.unexpected(w, r)
			return
		}
		writeJSON(w, http.StatusOK, page(r, s.blockedBy[n]))
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "commits" && parts[2] == "check-runs":
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(s.checks), "check_runs": page(r, s.checks)})
	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "actions" && parts[1] == "jobs" && parts[3] == "logs":
		id, err := strconv.ParseInt(parts[2], 10, 64)
		log, ok := s.logs[id]
		if err != nil || !ok {
			writeError(w, http.StatusNotFound, "Not Found", "https://docs.github.com/rest/actions/workflow-jobs#download-job-logs-for-a-workflow-run")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(log))
	default:
		s.unexpected(w, r)
	}
}

func (s *Server) unexpected(w http.ResponseWriter, r *http.Request) {
	s.t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL)
	writeError(w, http.StatusNotFound, "Not Found", "https://docs.github.com/rest")
}

// listPulls filters by state (open by default, as GitHub does) and by head
// ("owner:branch").
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, head := q.Get("state"), q.Get("head")
	if state == "" {
		state = "open"
	}
	found := []map[string]any{}
	for _, p := range s.pulls {
		if head != "" && head != s.owner+":"+p.Head {
			continue
		}
		if state != "all" && state != orDefault(p.State, "open") {
			continue
		}
		found = append(found, p.reply())
	}
	writeJSON(w, http.StatusOK, page(r, found))
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request) {
	var p Pull
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Title == "" || p.Head == "" || p.Base == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed", "https://docs.github.com/rest/pulls/pulls#create-a-pull-request")
		return
	}
	p = s.add(p)
	writeJSON(w, http.StatusCreated, p.reply())
	if s.onPR != nil {
		s.onPR()
	}
}

// add numbers p and stores it. It holds s.mu.
func (s *Server) add(p Pull) Pull {
	p.Number = len(s.pulls) + 1
	p.State = orDefault(p.State, "open")
	if p.URL == "" {
		p.URL = fmt.Sprintf("https://github.test/%s/pull/%d", s.repo, p.Number)
	}
	s.pulls = append(s.pulls, p)
	return p
}

func (s *Server) merge(w http.ResponseWriter, r *http.Request, number string) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > len(s.pulls) {
		writeError(w, http.StatusNotFound, "Not Found", mergeDocs)
		return
	}
	var body struct {
		Method string `json:"merge_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON", mergeDocs)
		return
	}
	p := &s.pulls[n-1]
	status := s.mergeStatus
	if status == 0 && p.State != "open" {
		status = http.StatusMethodNotAllowed
	}
	if status != 0 {
		writeError(w, status, mergeErrors[status], mergeDocs)
		return
	}
	s.merges++
	s.mergeMethod = body.Method
	p.State, p.Merged = "closed", true
	writeJSON(w, http.StatusOK, map[string]any{"sha": fmt.Sprintf("%040d", n), "merged": true, "message": "Pull Request successfully merged"})
}

// FailMerge makes every later merge call return status (405, 409, or 422)
// with GitHub's JSON error body for it.
func (s *Server) FailMerge(status int) {
	s.t.Helper()
	if _, ok := mergeErrors[status]; !ok {
		s.t.Fatalf("FailMerge(%d): GitHub's merge fails with 405, 409, or 422", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeStatus = status
}

// OnPR sets a function called, with the fake locked, after a pull request
// is created.
func (s *Server) OnPR(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onPR = f
}

// AddBlocker adds blocker to the issues that block issue.
func (s *Server) AddBlocker(issue int, blocker Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockedBy == nil {
		s.blockedBy = map[int][]Issue{}
	}
	s.blockedBy[issue] = append(s.blockedBy[issue], blocker)
}

// SeedPull stores p as if it had been opened, and returns it numbered.
func (s *Server) SeedPull(p Pull) Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(p)
}

// SetComments replaces the comments on issue.
func (s *Server) SetComments(issue int, bodies ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments[strconv.Itoa(issue)] = slices.Clone(bodies)
}

// SetChecks sets the check runs every commit has.
func (s *Server) SetChecks(runs []CheckRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = slices.Clone(runs)
}

// SetLog sets the log of Actions job id. A job without one is not found.
func (s *Server) SetLog(id int64, log string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs == nil {
		s.logs = map[int64]string{}
	}
	s.logs[id] = log
}

// Snapshot returns the pull requests and the comments by issue number.
func (s *Server) Snapshot() ([]Pull, map[string][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	comments := map[string][]string{}
	for k, v := range s.comments {
		comments[k] = slices.Clone(v)
	}
	return slices.Clone(s.pulls), comments
}

// Calls returns every call so far as "METHOD /path".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Merges returns how many merges landed and the last merge method.
func (s *Server) Merges() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.merges, s.mergeMethod
}
