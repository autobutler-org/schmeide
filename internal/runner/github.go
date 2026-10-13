package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// github is the slice of the GitHub REST API a job needs, for one repository
// and with that job's token.
type github struct {
	api, repo, token string
}

type ghIssue struct {
	Title string `json:"title"`
	Body  string `json:"body"`
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

func (g github) issue(ctx context.Context, number int) (ghIssue, error) {
	var issue ghIssue
	return issue, g.do(ctx, http.MethodGet, fmt.Sprintf("/issues/%d", number), nil, &issue)
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

// findPR returns the URL of a pull request, open or not, on a branch of the
// issue ("<issue number>_..."), or "" when there is none.
//
// ponytail: looks at the 100 newest pull requests only, which covers a
// redelivery minutes after the first attempt; page through them if a job can
// come back after 100 other pull requests were opened.
func (g github) findPR(ctx context.Context, issue int) (string, error) {
	var pulls []ghPull
	if err := g.do(ctx, http.MethodGet, "/pulls?state=all&per_page=100", nil, &pulls); err != nil {
		return "", err
	}
	for _, p := range pulls {
		if strings.HasPrefix(p.Head.Ref, fmt.Sprintf("%d_", issue)) {
			return p.HTMLURL, nil
		}
	}
	return "", nil
}

func (g github) openPR(ctx context.Context, title, body, head, base string) (string, error) {
	var pull ghPull
	in := map[string]string{"title": title, "body": body, "head": head, "base": base}
	return pull.HTMLURL, g.do(ctx, http.MethodPost, "/pulls", in, &pull)
}

func (g github) comment(ctx context.Context, issue int, body string) error {
	return g.do(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", issue), map[string]string{"body": body}, nil)
}
