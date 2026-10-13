// Package control is the schmeide control node: it turns GitHub issue webhooks
// into jobs on an embedded NATS JetStream queue and mints per-job GitHub tokens.
package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DefaultReadyLabel is the label that, together with an assignee, makes an
// issue a job.
const DefaultReadyLabel = "schmeide:ready"

// ErrBadSignature is returned when a webhook's X-Hub-Signature-256 header is
// missing or does not match the payload.
var ErrBadSignature = errors.New("webhook signature is missing or invalid")

// Job is the message the control node publishes and a runner consumes: one
// GitHub issue to be taken to a pull request.
type Job struct {
	// Repo is the repository's full name, "owner/name".
	Repo string `json:"repo"`
	// RepoID is GitHub's numeric repository ID.
	RepoID int64 `json:"repo_id"`
	// Issue is the issue number within the repository.
	Issue int `json:"issue"`
	// Steward is the login of the issue assignee, who owns the job.
	Steward string `json:"steward"`
	// InstallationID is the GitHub App installation the webhook came from.
	InstallationID int64 `json:"installation_id"`
}

// Subject is the JetStream subject the job is published on. A runner pulls its
// steward's jobs with the filter "schmeide.jobs.<steward>.>".
func (j Job) Subject() string {
	return fmt.Sprintf("schmeide.jobs.%s.%d.%d", j.Steward, j.RepoID, j.Issue)
}

// VerifySignature checks a GitHub X-Hub-Signature-256 header value against the
// payload and the App's webhook secret.
func VerifySignature(secret, body []byte, header string) error {
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || !strings.HasPrefix(header, "sha256=") {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrBadSignature
	}
	return nil
}

// JobFromIssuesEvent returns the job for a GitHub `issues` webhook payload, or
// nil when the issue is not an open, assigned, ready-labeled issue.
func JobFromIssuesEvent(body []byte, readyLabel string) (*Job, error) {
	type user struct {
		Login string `json:"login"`
	}
	var ev struct {
		Issue struct {
			Number    int    `json:"number"`
			State     string `json:"state"`
			Assignee  *user  `json:"assignee"`
			Assignees []user `json:"assignees"`
			Labels    []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"issue"`
		Repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("decode issues event: %w", err)
	}

	steward := ""
	switch {
	case ev.Issue.Assignee != nil:
		steward = ev.Issue.Assignee.Login
	case len(ev.Issue.Assignees) > 0:
		steward = ev.Issue.Assignees[0].Login
	}
	ready := false
	for _, l := range ev.Issue.Labels {
		ready = ready || l.Name == readyLabel
	}
	if steward == "" || !ready || ev.Issue.State != "open" {
		return nil, nil //nolint:nilnil // no job is a normal outcome, not an error
	}
	// The steward becomes a NATS subject token, so it must not carry subject syntax.
	if strings.ContainsAny(steward, ".*> \t\r\n") {
		return nil, fmt.Errorf("assignee login %q cannot be used as a steward", steward)
	}
	if ev.Issue.Number == 0 || ev.Repository.ID == 0 || ev.Installation.ID == 0 {
		return nil, errors.New("issues event is missing the issue number, repository id, or installation id")
	}
	return &Job{
		Repo:           ev.Repository.FullName,
		RepoID:         ev.Repository.ID,
		Issue:          ev.Issue.Number,
		Steward:        steward,
		InstallationID: ev.Installation.ID,
	}, nil
}

// Outcome is what Intake did with a webhook delivery.
type Outcome string

// The outcomes of a webhook delivery.
const (
	OutcomeIgnored   Outcome = "ignored"
	OutcomeCreated   Outcome = "created"
	OutcomeDuplicate Outcome = "duplicate"
)

// Intake turns verified webhook deliveries into jobs.
type Intake struct {
	// Secret is the GitHub App's webhook secret.
	Secret []byte
	// ReadyLabel is the label that marks an issue ready.
	ReadyLabel string
	// Publish stores a job and reports whether it is new. It must be idempotent
	// per issue.
	Publish func(ctx context.Context, job Job) (created bool, err error)
}

// Handle verifies one webhook delivery and publishes a job if it describes an
// assigned, ready-labeled issue. event is the X-GitHub-Event header and
// signature is the X-Hub-Signature-256 header.
func (in Intake) Handle(ctx context.Context, event, signature string, body []byte) (Outcome, error) {
	if err := VerifySignature(in.Secret, body, signature); err != nil {
		return "", err
	}
	if event != "issues" {
		return OutcomeIgnored, nil
	}
	job, err := JobFromIssuesEvent(body, in.ReadyLabel)
	if err != nil {
		return "", err
	}
	if job == nil {
		return OutcomeIgnored, nil
	}
	created, err := in.Publish(ctx, *job)
	if err != nil {
		return "", fmt.Errorf("publish job for %s#%d: %w", job.Repo, job.Issue, err)
	}
	if !created {
		return OutcomeDuplicate, nil
	}
	return OutcomeCreated, nil
}
