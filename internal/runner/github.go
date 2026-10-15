package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// github is the slice of the GitHub REST API a job needs, for one repository
// and with that job's token.
type github struct {
	api, repo, token string
}

// do calls one endpoint under /repos/<repo>.
func (g github) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.api, "/")+"/repos/"+g.repo+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (g github) defaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	return repo.DefaultBranch, g.do(ctx, http.MethodGet, "", nil, &repo)
}

type ghPull struct {
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// pullPageSize is how many pull requests one list page holds. A shorter page
// is the last page.
const pullPageSize = 100

// findPR returns the URL of a pull request, open or not, on a branch of the
// issue ("<issue number>_..."), or "" when there is none. The match is the
// issue-number prefix, so a retitle of the issue still finds the pull request.
// Pages are followed until that branch is found or a page comes back short.
func (g github) findPR(ctx context.Context, issue int) (string, error) {
	prefix := fmt.Sprintf("%d_", issue)
	for page := 1; ; page++ {
		var pulls []ghPull
		path := fmt.Sprintf("/pulls?state=all&per_page=%d&page=%d", pullPageSize, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &pulls); err != nil {
			return "", err
		}
		for _, p := range pulls {
			if strings.HasPrefix(p.Head.Ref, prefix) {
				return p.HTMLURL, nil
			}
		}
		if len(pulls) < pullPageSize {
			return "", nil
		}
	}
}

func (g github) openPR(ctx context.Context, title, body, head, base string) (string, error) {
	var pull ghPull
	in := map[string]string{"title": title, "body": body, "head": head, "base": base}
	return pull.HTMLURL, g.do(ctx, http.MethodPost, "/pulls", in, &pull)
}

func (g github) comment(ctx context.Context, issue int, body string) error {
	return g.do(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", issue), map[string]string{"body": body}, nil)
}

// ghCheckRun is one check run on a commit. Status is completed once GitHub
// has a conclusion.
type ghCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// checkPageSize is how many check runs one list page holds. A shorter page
// is the last page.
const checkPageSize = 100

// checkRuns lists the latest check run for each name on sha. Pages are
// followed until a short page comes back.
func (g github) checkRuns(ctx context.Context, sha string) ([]ghCheckRun, error) {
	var all []ghCheckRun
	for page := 1; ; page++ {
		if page > 20 {
			return nil, fmt.Errorf("too many check run pages for %s", sha)
		}
		var resp struct {
			TotalCount int          `json:"total_count"`
			CheckRuns  []ghCheckRun `json:"check_runs"`
		}
		q := url.Values{}
		q.Set("filter", "latest")
		q.Set("per_page", strconv.Itoa(checkPageSize))
		q.Set("page", strconv.Itoa(page))
		endpoint := "/commits/" + url.PathEscape(sha) + "/check-runs?" + q.Encode()
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.CheckRuns...)
		if len(resp.CheckRuns) < checkPageSize || (resp.TotalCount > 0 && len(all) >= resp.TotalCount) {
			return all, nil
		}
	}
}

// mergePull merges the pull request with a merge commit. It does not squash
// or rebase.
func (g github) mergePull(ctx context.Context, number int) error {
	body := map[string]string{"merge_method": "merge"}
	return g.do(ctx, http.MethodPut, fmt.Sprintf("/pulls/%d/merge", number), body, nil)
}

// pullNumber is the pull request number at the end of a GitHub html URL.
func pullNumber(prURL string) (int, error) {
	u, err := url.Parse(prURL)
	if err != nil {
		return 0, fmt.Errorf("pull request URL %q: %w", prURL, err)
	}
	n, err := strconv.Atoi(path.Base(u.Path))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("pull request URL %q has no pull number", prURL)
	}
	return n, nil
}
