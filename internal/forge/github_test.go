package forge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	testRepo  = "o/r"
	testToken = "job-token"
)

// TestFindPRPagesPastTheNewest finds a pull request on a later page. The head
// ref only shares the issue-number prefix, which is what survives a retitle.
func TestFindPRPagesPastTheNewest(t *testing.T) {
	const want = "https://github.test/o/r/pull/7"
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+testRepo+"/pulls" {
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != "all" {
			t.Errorf("state = %q, want all", q.Get("state"))
		}
		perPage, err := strconv.Atoi(q.Get("per_page"))
		if err != nil || perPage < 1 {
			t.Errorf("per_page = %q", q.Get("per_page"))
			perPage = 100
		}
		page := q.Get("page")
		if page == "" {
			page = "1"
		}
		pages = append(pages, page)
		var pulls []Pull
		switch page {
		case "1":
			for i := range perPage {
				p := Pull{HTMLURL: fmt.Sprintf("https://github.test/o/r/pull/%d", 1000+i)}
				// 70_newer must not match issue 7. Only a "7_" prefix does.
				p.Head.Ref = fmt.Sprintf("%d_newer", 70+i)
				pulls = append(pulls, p)
			}
		case "2":
			p := Pull{HTMLURL: want, MergedAt: "2026-10-16T00:00:00Z"}
			p.Head.Ref = "7_previous-title"
			pulls = append(pulls, p)
		}
		if err := json.NewEncoder(w).Encode(pulls); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	got, ok, err := (GitHub{API: srv.URL, Repo: testRepo, Token: StaticToken(testToken)}).IssuePull(t.Context(), 7, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.HTMLURL != want || got.MergedAt != "2026-10-16T00:00:00Z" {
		t.Errorf("IssuePull = %q merged at %q, %v, want %q merged", got.HTMLURL, got.MergedAt, ok, want)
	}
	if !slices.Equal(pages, []string{"1", "2"}) {
		t.Errorf("pages = %v, want the first page and the next", pages)
	}
}

func TestProtectionReadsRequiredReviews(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"reviews", http.StatusOK, `{"required_pull_request_reviews":{"required_approving_review_count":1}}`, 1},
		{"status checks only", http.StatusOK, `{"required_status_checks":{"strict":true,"contexts":["ci"]}}`, 0},
		{"not protected", http.StatusNotFound, `{"message":"Branch not protected"}`, 0},
		{"no access", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/"+testRepo+"/branches/feat/x/protection" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			got, err := (GitHub{API: srv.URL, Repo: testRepo, Token: StaticToken(testToken)}).Protection(t.Context(), "feat/x")
			if err != nil || got.RequiredApprovals != tt.want {
				t.Fatalf("Protection = %+v, %v, want %d approvals", got, err, tt.want)
			}
		})
	}
}

func TestProtectionFailsOnAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	if _, err := (GitHub{API: srv.URL, Repo: testRepo, Token: StaticToken(testToken)}).Protection(t.Context(), "main"); err == nil {
		t.Fatal("Protection on a 502 = nil error, want the failure")
	}
}

// TestJobLogKeepsTheTail follows the redirect to the log text and keeps the
// end of a log longer than the limit. A shorter log comes back whole.
func TestJobLogKeepsTheTail(t *testing.T) {
	old := logReadLimit
	logReadLimit = 16
	t.Cleanup(func() { logReadLimit = old })
	long := strings.Repeat("head ", 20) + "FAIL: TestThing"
	for _, tc := range []struct{ body, want string }{
		{"short log", "short log"},
		{long, long[len(long)-16:]},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/" + testRepo + "/actions/jobs/9/logs":
				http.Redirect(w, r, "/log", http.StatusFound)
			case "/log":
				fmt.Fprint(w, tc.body)
			default:
				t.Errorf("path = %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		got, err := (GitHub{API: srv.URL, Repo: testRepo, Token: StaticToken(testToken)}).JobLog(t.Context(), 9)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("JobLog = %q, want %q", got, tc.want)
		}
	}
}
