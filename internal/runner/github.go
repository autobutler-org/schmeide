package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// ghIssue is the part of an issue read from the issue-dependencies API.
type ghIssue struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// blockedBy lists the issues that block issue. The list is GitHub's
// blocked-by relationship. No blockers is an empty list. Pages are followed
// until a short page comes back.
func (g github) blockedBy(ctx context.Context, issue int) ([]ghIssue, error) {
	var all []ghIssue
	for page := 1; ; page++ {
		if page > 20 {
			return nil, fmt.Errorf("too many blocked-by pages for issue %d", issue)
		}
		var batch []ghIssue
		endpoint := fmt.Sprintf("/issues/%d/dependencies/blocked_by?per_page=%d&page=%d", issue, pullPageSize, page)
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < pullPageSize {
			return all, nil
		}
	}
}

// baseBranch is the branch a new pull request for issue is opened against,
// and the branch its worktree is created from. It is the repository default
// branch when no open blocker has an open schmeide pull request: there is no
// blocker, every blocker is already merged, or a blocker has no open pull
// request whose branch starts with that issue's "<number>_". Exactly one
// such pull request selects its branch. More than one is refused.
func (g github) baseBranch(ctx context.Context, issue int) (string, error) {
	base, err := g.defaultBranch(ctx)
	if err != nil {
		return "", err
	}
	blockers, err := g.blockedBy(ctx, issue)
	if err != nil {
		return "", err
	}
	stacked := ""
	for _, blocker := range blockers {
		if blocker.State != "open" {
			continue
		}
		branch, err := g.openIssueBranch(ctx, blocker.Number)
		if err != nil {
			return "", err
		}
		if branch == "" {
			continue
		}
		if stacked != "" {
			return "", errors.New("only one open dependency is supported")
		}
		stacked = branch
	}
	if stacked != "" {
		return stacked, nil
	}
	return base, nil
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
	pr, ok, err := g.issuePull(ctx, issue, "all")
	if err != nil || !ok {
		return "", err
	}
	return pr.HTMLURL, nil
}

// openIssueBranch returns the head branch of an open pull request whose
// branch starts with "<issue>_", or "" when there is none.
func (g github) openIssueBranch(ctx context.Context, issue int) (string, error) {
	pr, ok, err := g.issuePull(ctx, issue, "open")
	if err != nil || !ok {
		return "", err
	}
	return pr.Head.Ref, nil
}

// listPulls returns one page of pull requests in state ("open", "closed", or "all").
func (g github) listPulls(ctx context.Context, state string, page int) ([]ghPull, error) {
	var pulls []ghPull
	endpoint := fmt.Sprintf("/pulls?state=%s&per_page=%d&page=%d", state, pullPageSize, page)
	err := g.do(ctx, http.MethodGet, endpoint, nil, &pulls)
	return pulls, err
}

// issuePull returns the first pull request in state whose head ref starts
// with "<issue>_". ok is false when there is none.
func (g github) issuePull(ctx context.Context, issue int, state string) (ghPull, bool, error) {
	prefix := fmt.Sprintf("%d_", issue)
	for page := 1; ; page++ {
		pulls, err := g.listPulls(ctx, state, page)
		if err != nil {
			return ghPull{}, false, err
		}
		for _, p := range pulls {
			if strings.HasPrefix(p.Head.Ref, prefix) {
				return p, true, nil
			}
		}
		if len(pulls) < pullPageSize {
			return ghPull{}, false, nil
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
