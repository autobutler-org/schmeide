package forge

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

// graphql runs one query against the GitHub GraphQL API. The merge queue is
// only in GraphQL; REST has no endpoint for it.
func (g GitHub) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	data, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	// api.github.com serves /graphql; GitHub Enterprise serves /api/graphql
	// next to /api/v3.
	endpoint := strings.TrimSuffix(strings.TrimSuffix(g.API, "/"), "/v3") + "/graphql"
	auth, err := g.Bearer(ctx)
	if err != nil {
		return fmt.Errorf("github graphql: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github graphql: %w", err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("github graphql: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github graphql: %s: %s", resp.Status, data)
	}
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("github graphql: %w", err)
	}
	if len(body.Errors) > 0 {
		return fmt.Errorf("github graphql: %s", body.Errors[0].Message)
	}
	return json.Unmarshal(body.Data, out)
}

// QueuedPull reads the base branch with GraphQL: REST has no merge queue.
func (g GitHub) QueuedPull(ctx context.Context, number int) (string, error) {
	owner, name, _ := strings.Cut(g.Repo, "/")
	var pr struct {
		Repository struct {
			PullRequest struct {
				ID          string `json:"id"`
				BaseRefName string `json:"baseRefName"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	const prQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) { id baseRefName } }
}`
	if err := g.graphql(ctx, prQuery, map[string]any{"owner": owner, "name": name, "number": number}, &pr); err != nil {
		return "", err
	}
	var queue struct {
		Repository struct {
			MergeQueue *struct {
				ID string `json:"id"`
			} `json:"mergeQueue"`
		} `json:"repository"`
	}
	const queueQuery = `query($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) { mergeQueue(branch: $branch) { id } }
}`
	if err := g.graphql(ctx, queueQuery, map[string]any{"owner": owner, "name": name, "branch": pr.Repository.PullRequest.BaseRefName}, &queue); err != nil {
		return "", err
	}
	if queue.Repository.MergeQueue == nil {
		return "", nil
	}
	return pr.Repository.PullRequest.ID, nil
}

// EnqueuePull uses GraphQL, the only API with the merge queue.
func (g GitHub) EnqueuePull(ctx context.Context, id string) error {
	const mutation = `mutation($id: ID!) {
  enqueuePullRequest(input: {pullRequestId: $id}) { mergeQueueEntry { id } }
}`
	var out json.RawMessage
	return g.graphql(ctx, mutation, map[string]any{"id": id}, &out)
}
