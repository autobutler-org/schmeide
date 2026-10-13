// Package runner is the schmeide runner: it pulls its steward's jobs from the
// control node and takes each one from a GitHub issue to an opened pull
// request, using a harness that is already logged in on this machine.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/repoconfig"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// RegisterSubjectPrefix plus a steward is the subject a runner announces
	// itself on when it starts. The message is a Registration.
	RegisterSubjectPrefix = "schmeide.runners."
	// StatusSubjectPrefix replaces "schmeide.jobs." in a job's subject to give
	// the subject its Status messages are published on.
	StatusSubjectPrefix = "schmeide.status."

	jobSubjectPrefix = "schmeide.jobs."
	// PRTextPath is where the harness leaves the pull request title and body,
	// relative to the checkout. It is never committed.
	prTextPath = ".schmeide/pr.md"

	defaultAckWait = 30 * time.Second
	pollWait       = time.Second
	retryDelay     = 30 * time.Second
	gateTailBytes  = 4000
)

// State is where a job stands. Gate failed, PR opened, and failed are final.
type State string

// The states a runner reports for a job.
const (
	StateClaimed    State = "claimed"
	StateWorking    State = "working"
	StateGateFailed State = "gate_failed"
	StatePROpened   State = "pr_opened"
	StateFailed     State = "failed"
)

// Status is the message a runner publishes when a job changes state.
type Status struct {
	State State `json:"state"`
	// Detail is the PR URL for StatePROpened and the reason for the failure
	// states.
	Detail string `json:"detail,omitempty"`
}

// Registration is the message a runner publishes when it starts.
type Registration struct {
	Steward   string   `json:"steward"`
	Harnesses []string `json:"harnesses"`
	Host      string   `json:"host"`
}

// Options configures a runner.
type Options struct {
	// NATSURL is the control node's NATS URL.
	NATSURL string
	// NATSToken is the token the control node's NATS expects, if any.
	NATSToken string
	// Steward is the GitHub login whose jobs this runner takes.
	Steward string
	// WorkDir is where repository clones and job worktrees are kept.
	WorkDir string
	// GitHubAPIURL is the GitHub REST API base URL.
	GitHubAPIURL string
	// GitHubURL is the base URL repositories are cloned from and pushed to.
	GitHubURL string
	// GitName and GitEmail are the identity commits are made under.
	GitName, GitEmail string
	// Harnesses are the harnesses to offer. The first one that passes its
	// check runs the jobs.
	Harnesses []harness.Harness
	// Output receives harness session output. Nil discards it.
	Output io.Writer
	// AckWait is how long the control node waits without a heartbeat before
	// it redelivers a job. Zero means 30 seconds.
	AckWait time.Duration
}

// Runner pulls and works one steward's jobs.
type Runner struct {
	opts    Options
	log     *slog.Logger
	nc      *nats.Conn
	cons    jetstream.Consumer
	harness harness.Harness
}

// Connect opens a connection to the control node's NATS and checks that the
// job stream is there. The error says what to check.
func Connect(ctx context.Context, url, token string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url, nats.Token(token), nats.Name("schmeide runner"), nats.MaxReconnects(-1))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the control node's NATS at %s: %w\n"+
			"Check that `schmeide control` is running, that --nats-url points at its --nats-addr, "+
			"and that --nats-token matches its --nats-token", url, err)
	}
	js, err := jetstream.New(nc)
	if err == nil {
		_, err = js.Stream(ctx, control.JobStream)
	}
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("connected to NATS at %s but could not find the job stream %s: %w\n"+
			"Check that --nats-url points at a `schmeide control` node and not at some other NATS server",
			url, control.JobStream, err)
	}
	return nc, js, nil
}

// New connects to the control node, checks this machine can do work, and
// registers the runner under its steward.
func New(ctx context.Context, o Options, log *slog.Logger) (*Runner, error) {
	// The steward becomes a NATS subject token and a consumer name.
	if o.Steward == "" || strings.ContainsAny(o.Steward, ".*>/\\ \t\r\n") {
		return nil, fmt.Errorf("steward %q is not a GitHub login; pass your login with --steward", o.Steward)
	}
	if o.AckWait == 0 {
		o.AckWait = defaultAckWait
	}
	var err error
	if o.WorkDir, err = filepath.Abs(o.WorkDir); err != nil {
		return nil, fmt.Errorf("work directory: %w", err)
	}

	nc, js, err := Connect(ctx, o.NATSURL, o.NATSToken)
	if err != nil {
		return nil, err
	}
	r := &Runner{opts: o, log: log, nc: nc}
	if err := r.register(ctx, js); err != nil {
		nc.Close()
		return nil, err
	}
	return r, nil
}

func (r *Runner) register(ctx context.Context, js jetstream.JetStream) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git was not found on PATH; install git, then run `schmeide doctor`")
	}
	var names []string
	var problems []string
	for _, h := range r.opts.Harnesses {
		if err := h.Check(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("  %s: %v", h.Name(), err))
			continue
		}
		names = append(names, h.Name())
		if r.harness == nil {
			r.harness = h
		}
	}
	if r.harness == nil {
		return fmt.Errorf("no usable harness on this machine:\n%s\nFix one of them, then run `schmeide doctor`",
			strings.Join(problems, "\n"))
	}

	var err error
	// One durable consumer per steward, shared by all of the steward's
	// runners, so each job goes to exactly one of them and no other steward's
	// job is ever delivered here.
	r.cons, err = js.CreateOrUpdateConsumer(ctx, control.JobStream, jetstream.ConsumerConfig{
		Durable:       "runner-" + r.opts.Steward,
		FilterSubject: jobSubjectPrefix + r.opts.Steward + ".>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       r.opts.AckWait,
	})
	if err != nil {
		return fmt.Errorf("subscribe to %s's jobs: %w", r.opts.Steward, err)
	}
	host, _ := os.Hostname()
	r.publish(RegisterSubjectPrefix+r.opts.Steward, Registration{Steward: r.opts.Steward, Harnesses: names, Host: host})
	r.log.Info("runner registered", "steward", r.opts.Steward, "harnesses", strings.Join(names, ","), "nats", r.opts.NATSURL)
	return nil
}

// Close drops the connection to the control node.
func (r *Runner) Close() { r.nc.Close() }

// Run works jobs one at a time until ctx is canceled or the connection to the
// control node is closed.
//
// ponytail: one job at a time per runner; run jobs concurrently (each already
// has its own worktree) when a steward's queue backs up.
func (r *Runner) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		msg, err := r.cons.Next(jetstream.FetchMaxWait(pollWait))
		switch {
		case err == nil:
			r.handle(ctx, msg)
		case errors.Is(err, nats.ErrTimeout):
		case r.nc.IsClosed():
			return fmt.Errorf("lost the connection to the control node at %s: %w; check that `schmeide control` is still running, then start the runner again", r.opts.NATSURL, err)
		default:
			r.log.Warn("could not pull a job; retrying", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(pollWait):
			}
		}
	}
	return nil
}

func (r *Runner) publish(subject string, v any) {
	data, err := json.Marshal(v)
	if err == nil {
		err = r.nc.Publish(subject, data)
	}
	if err != nil {
		r.log.Warn("could not report to the control node", "subject", subject, "err", err)
	}
}

func (r *Runner) report(job control.Job, state State, detail string) {
	r.log.Info("job state", "repo", job.Repo, "issue", job.Issue, "state", string(state), "detail", detail)
	subject := StatusSubjectPrefix + strings.TrimPrefix(job.Subject(), jobSubjectPrefix)
	r.publish(subject, Status{State: state, Detail: detail})
}

// token asks the control node for the job's GitHub token.
func (r *Runner) token(ctx context.Context, subject string) (string, error) {
	msg, err := r.nc.RequestWithContext(ctx, control.TokenSubject, []byte(subject))
	if err != nil {
		return "", fmt.Errorf("ask the control node for the job's token: %w", err)
	}
	var reply control.TokenReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return "", fmt.Errorf("decode the job's token: %w", err)
	}
	if reply.Error != "" || reply.Token.Token == "" {
		return "", fmt.Errorf("the control node could not mint a token: %s", reply.Error)
	}
	return reply.Token.Token, nil
}

// handle works one delivered job. It acknowledges the job only once the job
// has reached a final state, so a runner that dies before then leaves the job
// to be redelivered.
func (r *Runner) handle(ctx context.Context, msg jetstream.Msg) {
	var job control.Job
	if err := json.Unmarshal(msg.Data(), &job); err != nil || job.Subject() != msg.Subject() {
		r.log.Error("dropping a malformed job", "subject", msg.Subject(), "err", err)
		_ = msg.Term()
		return
	}

	// Heartbeats keep the job ours while it is being worked.
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(r.opts.AckWait / 3)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_ = msg.InProgress()
			}
		}
	}()

	r.report(job, StateClaimed, "")
	token, err := r.token(ctx, msg.Subject())
	if err != nil {
		// Without a token the outcome cannot be put on the issue, so the job
		// is handed back instead of being failed where nobody would see it.
		r.log.Error("handing the job back", "repo", job.Repo, "issue", job.Issue, "err", err)
		_ = msg.NakWithDelay(retryDelay)
		return
	}
	gh := github{api: r.opts.GitHubAPIURL, repo: job.Repo, token: token}
	out := r.work(ctx, gh, job, token)
	if ctx.Err() != nil {
		_ = msg.Nak() // shutting down: let another runner have it now
		return
	}
	if out.comment != "" {
		if err := gh.comment(ctx, job.Issue, out.comment); err != nil {
			r.log.Error("could not comment on the issue", "repo", job.Repo, "issue", job.Issue, "err", err)
		}
	}
	r.report(job, out.state, out.detail)
	if err := msg.Ack(); err != nil {
		r.log.Error("could not acknowledge the job; it will be redelivered", "repo", job.Repo, "issue", job.Issue, "err", err)
	}
}

// outcome is the final state of one attempt at a job.
type outcome struct {
	state  State
	detail string
	// comment is posted on the issue. Empty posts nothing.
	comment string
}

func failed(err error) outcome {
	return outcome{
		state:   StateFailed,
		detail:  err.Error(),
		comment: "schmeide could not finish this job and did not open a pull request:\n\n" + err.Error(),
	}
}

// work takes the job as far as it goes on this attempt.
func (r *Runner) work(ctx context.Context, gh github, job control.Job, token string) outcome {
	issue, err := gh.issue(ctx, job.Issue)
	if err != nil {
		return failed(err)
	}
	base, err := gh.defaultBranch(ctx)
	if err != nil {
		return failed(err)
	}
	branch := BranchName(job.Issue, issue.Title)
	// A redelivered job whose first runner died after opening the PR must not
	// open a second one. The first attempt's PR already links the issue, so
	// nothing more is said there.
	//
	// ponytail: keyed on the branch name, so an issue retitled between two
	// attempts gets a second PR; key on a PR label or the status history if
	// that shows up.
	if url, err := gh.findPR(ctx, branch); err != nil {
		return failed(err)
	} else if url != "" {
		return outcome{state: StatePROpened, detail: url}
	}

	ws, err := newWorktree(ctx, worktreeSpec{
		workDir:  r.opts.WorkDir,
		url:      strings.TrimSuffix(r.opts.GitHubURL, "/") + "/" + job.Repo + ".git",
		base:     base,
		branch:   branch,
		token:    token,
		gitName:  r.opts.GitName,
		gitEmail: r.opts.GitEmail,
		id:       fmt.Sprintf("%d-%d", job.RepoID, job.Issue),
		repoID:   strconv.FormatInt(job.RepoID, 10),
	})
	if err != nil {
		return failed(err)
	}
	defer func() {
		if err := ws.Close(); err != nil {
			r.log.Warn("could not clean up the job's worktree", "repo", job.Repo, "issue", job.Issue, "err", err)
		}
	}()
	return r.execute(ctx, ws, gh, job, issue, branch, base)
}

// execute is the job itself, written against Workspace only.
func (r *Runner) execute(ctx context.Context, ws Workspace, gh github, job control.Job, issue ghIssue, branch, base string) outcome {
	// The gate is read before the session runs, so the session cannot weaken it.
	cfg, err := repoconfig.Load(ws.Dir())
	if err != nil {
		return failed(err)
	}
	r.report(job, StateWorking, r.harness.Name())
	req := harness.Request{Dir: ws.Dir(), Prompt: brief(job, issue, cfg.Gate), Output: r.opts.Output}
	if err := r.harness.Run(ctx, req); err != nil {
		return failed(fmt.Errorf("the %s session did not finish: %w", r.harness.Name(), err))
	}
	title, body := prText(ws.Dir(), job.Issue, issue.Title)
	changed, err := ws.Commit(ctx, title)
	if err != nil {
		return failed(err)
	}
	if !changed {
		return failed(fmt.Errorf("the %s session made no changes", r.harness.Name()))
	}
	if output, err := ws.Run(ctx, cfg.Gate); err != nil {
		if len(output) > gateTailBytes {
			output = "...\n" + output[len(output)-gateTailBytes:]
		}
		detail := fmt.Sprintf("the gate command `%s` failed: %v", cfg.Gate, err)
		return outcome{
			state:   StateGateFailed,
			detail:  detail,
			comment: fmt.Sprintf("schmeide did not open a pull request: %s\n\n```\n%s\n```", detail, output),
		}
	}
	if err := ws.Push(ctx); err != nil {
		return failed(err)
	}
	url, err := gh.openPR(ctx, title, body, branch, base)
	if err != nil {
		return failed(err)
	}
	return outcome{state: StatePROpened, detail: url, comment: "schmeide opened a pull request for this issue: " + url}
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

// BranchName is the job's branch: "<issue number>_<short-description>".
func BranchName(issue int, title string) string {
	slug := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "issue"
	}
	return fmt.Sprintf("%d_%s", issue, slug)
}

// brief is the prompt for the job's harness session.
func brief(job control.Job, issue ghIssue, gate string) string {
	return fmt.Sprintf(`You are working on GitHub issue #%[1]d of %[2]s, in a fresh checkout of the repository on its own branch.

Issue title: %[3]s

Issue body:
%[4]s

Make the change the issue asks for, with tests where the repository expects them. Follow the repository's own conventions (README, CONTRIBUTING, agent instruction files). Stay inside this directory. Do not push and do not open a pull request: that is done for you once the repository's gate command passes. The gate command is: %[5]s

When you are done, write the pull request text to %[6]s: the title on the first line, following the repository's commit and pull request title conventions, then a blank line, then the body, following the repository's pull request template if it has one. The body must contain the line "Closes #%[1]d".
`, job.Issue, job.Repo, issue.Title, issue.Body, gate, prTextPath)
}

// prText returns the PR title and body the session left in prTextPath, and
// removes the file so it is not committed. Without the file the issue title
// is used. The body always ends up with "Closes #N".
func prText(dir string, issue int, issueTitle string) (title, body string) {
	file := filepath.Join(dir, filepath.FromSlash(prTextPath))
	data, _ := os.ReadFile(file)
	_ = os.Remove(file)
	title, body, _ = strings.Cut(strings.TrimSpace(string(data)), "\n")
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title == "" {
		title = issueTitle
	}
	if closes := fmt.Sprintf("Closes #%d", issue); !strings.Contains(body, closes) {
		body = strings.TrimSpace(body + "\n\n" + closes)
	}
	return title, body
}
