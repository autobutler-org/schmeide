package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub is the slice of the GitHub REST and GraphQL APIs a job needs, for
// one repository and with that job's token.
type GitHub struct {
	// API is the REST base URL, such as https://api.github.com.
	API string
	// Repo is "owner/name".
	Repo string
	// Token returns the token for each request. The runner passes one that
	// refreshes the job's installation token before it expires.
	Token TokenSource
}

// TokenSource returns the token to send on the next request.
type TokenSource func(context.Context) (string, error)

// StaticToken is a TokenSource that always returns token.
func StaticToken(token string) TokenSource {
	return func(context.Context) (string, error) { return token, nil }
}

// Authorize sets the request's Authorization header from g.Token.
func (g GitHub) Authorize(req *http.Request) error {
	token, err := g.Token(req.Context())
	if err != nil {
		return fmt.Errorf("get the job's GitHub token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

var _ Forge = GitHub{}

// statusError is a reply outside 2xx.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// do calls one endpoint under /repos/<repo>.
func (g GitHub) do(ctx context.Context, method, path string, in, out any) error {
	data, err := g.send(ctx, method, path, in)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// send calls one endpoint under /repos/<repo> and returns the body of a 2xx
// reply. A redirect is followed.
func (g GitHub) send(ctx context.Context, method, path string, in any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.API, "/")+"/repos/"+g.Repo+path, body)
	if err != nil {
		return nil, err
	}
	if err := g.Authorize(req); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &statusError{code: resp.StatusCode, msg: fmt.Sprintf("github %s %s: %s: %s", method, path, resp.Status, data)}
	}
	return data, nil
}

func (g GitHub) DefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	return repo.DefaultBranch, g.do(ctx, http.MethodGet, "", nil, &repo)
}

// pageSize is how many items one list page holds. A shorter page is the
// last page.
const pageSize = 100

// BlockedBy follows pages until a short page comes back.
func (g GitHub) BlockedBy(ctx context.Context, issue int) ([]Issue, error) {
	var all []Issue
	for page := 1; ; page++ {
		if page > 20 {
			return nil, fmt.Errorf("too many blocked-by pages for issue %d", issue)
		}
		var batch []Issue
		endpoint := fmt.Sprintf("/issues/%d/dependencies/blocked_by?per_page=%d&page=%d", issue, pageSize, page)
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < pageSize {
			return all, nil
		}
	}
}

// IssuePull matches the issue-number prefix, so a retitle of the issue still
// finds the pull request. Pages are followed until that branch is found or a
// page comes back short.
func (g GitHub) IssuePull(ctx context.Context, issue int, state string) (Pull, bool, error) {
	prefix := fmt.Sprintf("%d_", issue)
	for page := 1; ; page++ {
		var pulls []Pull
		endpoint := fmt.Sprintf("/pulls?state=%s&per_page=%d&page=%d", state, pageSize, page)
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &pulls); err != nil {
			return Pull{}, false, err
		}
		for _, p := range pulls {
			if strings.HasPrefix(p.Head.Ref, prefix) {
				return p, true, nil
			}
		}
		if len(pulls) < pageSize {
			return Pull{}, false, nil
		}
	}
}

func (g GitHub) OpenPull(ctx context.Context, title, body, head, base string) (string, error) {
	var pull Pull
	in := map[string]string{"title": title, "body": body, "head": head, "base": base}
	return pull.HTMLURL, g.do(ctx, http.MethodPost, "/pulls", in, &pull)
}

func (g GitHub) Comment(ctx context.Context, issue int, body string) error {
	return g.do(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", issue), map[string]string{"body": body}, nil)
}

// Comments follows pages until a short page comes back.
func (g GitHub) Comments(ctx context.Context, issue int, since string) ([]Comment, error) {
	var all []Comment
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("/issues/%d/comments?per_page=%d&page=%d", issue, pageSize, page)
		if since != "" {
			endpoint += "&since=" + url.QueryEscape(since)
		}
		var batch []Comment
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < pageSize {
			return all, nil
		}
	}
}

// CheckRuns follows pages until a short page comes back.
func (g GitHub) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var all []CheckRun
	for page := 1; ; page++ {
		if page > 20 {
			return nil, fmt.Errorf("too many check run pages for %s", sha)
		}
		var resp struct {
			TotalCount int        `json:"total_count"`
			CheckRuns  []CheckRun `json:"check_runs"`
		}
		q := url.Values{}
		q.Set("filter", "latest")
		q.Set("per_page", strconv.Itoa(pageSize))
		q.Set("page", strconv.Itoa(page))
		endpoint := "/commits/" + url.PathEscape(sha) + "/check-runs?" + q.Encode()
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.CheckRuns...)
		if len(resp.CheckRuns) < pageSize || (resp.TotalCount > 0 && len(all) >= resp.TotalCount) {
			return all, nil
		}
	}
}

// logReadLimit is how much of a job log JobLog keeps. A var so a test can
// shrink it.
var logReadLimit = 8 << 20

// JobLog is answered with a redirect to the log text. It keeps the last
// logReadLimit bytes, since a failure is reported at the end of a log.
func (g GitHub) JobLog(ctx context.Context, id int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	path := fmt.Sprintf("/actions/jobs/%d/logs", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(g.API, "/")+"/repos/"+g.Repo+path, nil)
	if err != nil {
		return nil, err
	}
	if err := g.Authorize(req); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := readTail(resp.Body, logReadLimit)
	if err != nil {
		return nil, fmt.Errorf("github GET %s: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &statusError{code: resp.StatusCode, msg: fmt.Sprintf("github GET %s: %s: %s", path, resp.Status, data)}
	}
	return data, nil
}

// readTail reads r to the end and returns its last limit bytes. The buffer is
// cut back to limit whenever it passes twice that.
func readTail(r io.Reader, limit int) ([]byte, error) {
	var buf []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > 2*limit {
			buf = append(buf[:0], buf[len(buf)-limit:]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(buf) > limit {
		buf = buf[len(buf)-limit:]
	}
	return buf, nil
}

// MergePull does not squash or rebase.
func (g GitHub) MergePull(ctx context.Context, number int) error {
	body := map[string]string{"merge_method": "merge"}
	return g.do(ctx, http.MethodPut, fmt.Sprintf("/pulls/%d/merge", number), body, nil)
}

// Protection reads the branch protection endpoint, which needs
// Administration: read. GitHub answers 404 for a branch without protection.
// It answers 403 when the token cannot read protection, which happens when
// the App installation was not granted Administration: read; that is treated
// as no protection too, so merge goes on as it did before this read existed.
func (g GitHub) Protection(ctx context.Context, branch string) (Protection, error) {
	var resp struct {
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
	}
	endpoint := "/branches/" + strings.ReplaceAll(url.PathEscape(branch), "%2F", "/") + "/protection"
	err := g.do(ctx, http.MethodGet, endpoint, nil, &resp)
	var status *statusError
	if errors.As(err, &status) && (status.code == http.StatusNotFound || status.code == http.StatusForbidden) {
		return Protection{}, nil
	}
	if err != nil || resp.Reviews == nil {
		return Protection{}, err
	}
	return Protection{RequiredApprovals: resp.Reviews.Count}, nil
}
