// Package runner is the schmeide runner: it pulls its steward's jobs from the
// control node and takes each one from a GitHub issue to an opened pull
// request, then reviews that change in the same worktree until the review
// passes or the round limit is hit. When the issue is blocked by exactly one
// open issue that has an open pull request on a "<number>_" branch, the
// worktree and the new pull request use that branch as their base. It uses a
// harness that is already logged in on this machine. With --pool, which
// defaults to off, it also takes a job whose steward has no registered
// runner, one job at a time, its own first. The writer gets review fix
// rounds only when the repo config sets automate.review_fix = "on", and the
// factory merges the pull request only when it sets automate.merge = "on".
// The default for both is a person. A harness session is canceled after the session timeout (default 2
// hours) and the gate after the gate timeout (default 30 minutes). The repo
// config keys session_timeout and gate_timeout override those flags. A
// timeout cancels that process group, fails the job, and comments on the
// issue. The comment does not include the command's output. A job's GitHub
// token comes from the control node and is replaced with a new one when it
// is within five minutes of expiry, so a job may run longer than one token
// lives. The runner holds no other GitHub credential.
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

	"github.com/autobutler-org/schmeide/internal/ciwatch"
	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/forge"
	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/lintgate"
	"github.com/autobutler-org/schmeide/internal/release"
	"github.com/autobutler-org/schmeide/internal/repoconfig"
	"github.com/autobutler-org/schmeide/internal/trust"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// RegisterSubjectPrefix and StatusSubjectPrefix are the subjects a runner
	// reports to the control node on.
	RegisterSubjectPrefix = control.RegisterSubjectPrefix
	StatusSubjectPrefix   = control.StatusSubjectPrefix

	jobSubjectPrefix = "schmeide.jobs."
	// PRTextPath is where the harness leaves the pull request title and body,
	// relative to the checkout. It is never committed.
	prTextPath = ".schmeide/pr.md"

	defaultAckWait         = 5 * time.Minute
	defaultStuckAfter      = 3 * time.Hour
	defaultMaxAttempts     = 3
	defaultMaxReviewRounds = 3
	defaultRetryDelay      = 30 * time.Second
	gateTailBytes          = 4000
)

var pollWait = time.Second

// The messages a runner publishes are defined by the control node, which
// subscribes to them.
type (
	State        = control.State
	Status       = control.Status
	Registration = control.Registration
)

// The states a runner reports for a job.
const (
	StateClaimed    = control.StateClaimed
	StateWorking    = control.StateWorking
	StateRetrying   = control.StateRetrying
	StateGateFailed = control.StateGateFailed
	StatePROpened   = control.StatePROpened
	StateFailed     = control.StateFailed

	StateReviewing    = control.StateReviewing
	StateReviewFailed = control.StateReviewFailed
	StateMerged       = control.StateMerged
	StateQueued       = control.StateQueued
	StateMergeFailed  = control.StateMergeFailed
)

// Options configures a runner.
type Options struct {
	// NATSURL is the control node's NATS URL.
	NATSURL string
	// NATSToken is this steward's token, as listed for it in the control
	// node's runner tokens.
	NATSToken string
	// Steward is the GitHub login whose jobs this runner takes.
	Steward string
	// Version is sent in the registration. Empty means release.Version.
	Version string
	// Pool lets this runner take a job whose steward has no registered
	// runner. The default is false. The runner still works one job at a
	// time and takes its own steward's job first.
	Pool bool
	// WorkDir is where repository clones and job worktrees are kept.
	WorkDir string
	// GitHubAPIURL is the GitHub REST API base URL.
	GitHubAPIURL string
	// GitHubURL is the base URL repositories are cloned from and pushed to.
	GitHubURL string
	// GitName and GitEmail are the identity commits are made under.
	GitName, GitEmail string
	// PassEnv names variables to add for the harness session only, on top of
	// the base list and ANTHROPIC_, CLAUDE_, and XAI_. The gate does not use
	// this list: the gate inherits the runner process environment. Git sees
	// only the installation-token variables. Never list a secret.
	PassEnv []string
	// Harnesses are the harnesses to offer. The first one that passes its
	// check runs the jobs.
	Harnesses []harness.Harness
	// Output receives harness session output. Nil discards it.
	Output io.Writer
	// AckWait is how long the control node waits without a heartbeat before
	// it redelivers a job. Zero means 5 minutes.
	AckWait time.Duration
	// StuckAfter is how long a delivery may go without a job state change
	// before its work is canceled and it is handed back. Zero means 3 hours.
	StuckAfter time.Duration
	// MaxAttempts is how many StateFailed outcomes are allowed for
	// one job before that failure is final. A hand-back for a missing token
	// or a shutdown is not one of them. Zero means 3. The count is persisted
	// with the job's status, so a restarted process continues it.
	MaxAttempts int
	// MaxReviewRounds is how many reviewer sessions run after the pull
	// request is open. Zero means 3. Exhausting them fails the job and leaves
	// the pull request open.
	MaxReviewRounds int
	// RetryDelay is how long a failed job waits before it is delivered
	// again. Zero means 30 seconds.
	RetryDelay time.Duration
	// SessionTimeout is how long one harness session may run before its
	// process group is canceled. Zero means 2 hours. The repo config
	// session_timeout overrides a positive value when that key is set.
	SessionTimeout time.Duration
	// GateTimeout is how long the gate command may run before its process
	// group is canceled. Zero means 30 minutes. The repo config gate_timeout
	// overrides a positive value when that key is set. A gate timeout is not
	// retried.
	GateTimeout time.Duration
	// Confirm asks the operator a yes-or-no question before the first job in
	// a new pair of work directory and origin URL. Nil means nobody can be
	// asked, so an untrusted pair is refused unless SCHMEIDE_TRUST=1 is set.
	Confirm func(question string) (bool, error)
}

// Runner pulls and works one steward's jobs.
type Runner struct {
	opts     Options
	log      *slog.Logger
	nc       *nats.Conn
	cons     jetstream.Consumer
	poolCons jetstream.Consumer
	// ackWait is the shortest AckWait of the consumers this runner pulls
	// from. It sets the heartbeat interval, and differs from
	// Options.AckWait when another runner created a consumer first.
	ackWait  time.Duration
	statuses jetstream.Stream
	harness  harness.Harness
	// attempts is restored from the status stream for the current job. Only
	// handle writes it, and Run works one job at a time.
	attempts int
	// failures is the same-process fallback when the status stream is absent
	// or temporarily unreadable.
	failures map[string]int
	// progress receives a signal whenever report publishes a state for the
	// current job. Only one handle runs at a time.
	progress chan struct{}
	// stages is control.StageBucket. Nil when the control node has none, and
	// then no stage is recorded.
	stages jetstream.KeyValue
	// stage is how far the current job got. Only the job's goroutine uses it.
	stage stage
}

// The stages a job records. A job recorded at stagePROpened or stageReview
// resumes at its review after a restart. Any earlier stage starts over.
const (
	stageWorktreeReady   = "worktree_ready"
	stageSessionFinished = "session_finished"
	stageGatePassed      = "gate_passed"
	stagePROpened        = "pr_opened"
	stageReview          = "review"
)

// stage is the record kept in control.StageBucket for one job.
type stage struct {
	Stage  string `json:"stage"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	// PR is the open pull request's URL, and Opened when it was opened, in
	// RFC 3339.
	PR     string `json:"pr,omitempty"`
	Opened string `json:"opened,omitempty"`
	// Round is the last review round started.
	Round int `json:"round,omitempty"`
}

// Connect opens a connection to the control node's NATS as the steward and
// checks that a control node answers the steward's token requests. The error
// says what to check.
func Connect(ctx context.Context, url, steward, token string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url, nats.UserInfo(steward, token), nats.CustomInboxPrefix(control.InboxPrefix(steward)),
		nats.Name("schmeide runner"), nats.MaxReconnects(-1))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the control node's NATS at %s as %s: %w\n"+
			"Check that `schmeide control` is running, that --nats-url points at its --nats-addr, "+
			"and that --nats-token is the token its --runner-tokens lists for %s", url, steward, err, steward)
	}
	// An empty token request is refused, but only a control node answers it.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	js, err := jetstream.New(nc)
	if err == nil {
		_, err = nc.RequestWithContext(ctx, control.TokenSubjectPrefix+steward, nil)
	}
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("connected to NATS at %s but no control node answered for %s: %w\n"+
			"Check that --nats-url points at a `schmeide control` node and not at some other NATS server",
			url, steward, err)
	}
	return nc, js, nil
}

// New connects to the control node, checks this machine can do work, and
// registers the runner under its steward.
func New(ctx context.Context, o Options, log *slog.Logger) (*Runner, error) {
	// The steward becomes a NATS subject token and a consumer name. PoolName
	// is reserved so a steward's filter cannot match the pool subject.
	if o.Steward == "" || o.Steward == control.PoolName || strings.ContainsAny(o.Steward, ".*>/\\ \t\r\n") {
		return nil, fmt.Errorf("steward %q is not a GitHub login; pass your login with --steward", o.Steward)
	}
	if o.Version == "" {
		o.Version = release.Version
	}
	if o.AckWait == 0 {
		o.AckWait = defaultAckWait
	}
	if o.AckWait < 0 {
		return nil, fmt.Errorf("ack wait must be positive, got %s", o.AckWait)
	}
	if o.StuckAfter == 0 {
		o.StuckAfter = defaultStuckAfter
	}
	if o.StuckAfter < 0 {
		return nil, fmt.Errorf("stuck after must be positive, got %s", o.StuckAfter)
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.MaxReviewRounds <= 0 {
		o.MaxReviewRounds = defaultMaxReviewRounds
	}
	if o.RetryDelay == 0 {
		o.RetryDelay = defaultRetryDelay
	}
	if o.SessionTimeout <= 0 {
		o.SessionTimeout = repoconfig.DefaultSessionTimeout
	}
	if o.GateTimeout <= 0 {
		o.GateTimeout = repoconfig.DefaultGateTimeout
	}
	var err error
	if o.WorkDir, err = filepath.Abs(o.WorkDir); err != nil {
		return nil, fmt.Errorf("work directory: %w", err)
	}

	nc, js, err := Connect(ctx, o.NATSURL, o.Steward, o.NATSToken)
	if err != nil {
		return nil, err
	}
	r := &Runner{opts: o, log: log, nc: nc, failures: map[string]int{}}
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
	if r.cons, err = r.sharedConsumer(ctx, js, r.opts.Steward); err != nil {
		return fmt.Errorf("subscribe to %s's jobs: %w", r.opts.Steward, err)
	}
	r.ackWait = r.cons.CachedInfo().Config.AckWait
	if r.opts.Pool {
		// One durable consumer for every pool runner, so each pool job is
		// delivered to only one of them.
		if r.poolCons, err = r.sharedConsumer(ctx, js, control.PoolName); err != nil {
			return fmt.Errorf("subscribe to pool jobs: %w", err)
		}
		r.ackWait = min(r.ackWait, r.poolCons.CachedInfo().Config.AckWait)
	}
	if r.ackWait != r.opts.AckWait {
		r.log.Warn("a shared job consumer already has another ack wait; heartbeating at the consumer's",
			"ack_wait", r.opts.AckWait, "consumer_ack_wait", r.ackWait)
	}
	// An older control node may not have started status tracking yet. Jobs can
	// still run, but their attempt count cannot survive a runner restart.
	if r.statuses, err = js.Stream(ctx, control.StatusStream); err != nil {
		r.log.Warn("the control node keeps no job status history; upgrade it so retries survive runner restarts", "err", err)
		r.statuses = nil
	}
	// An older control node has no stage bucket. Jobs still run, and GitHub
	// alone finds an open pull request after a restart.
	if r.stages, err = js.KeyValue(ctx, control.StageBucket); err != nil {
		r.log.Warn("the control node keeps no job stages; upgrade it so a restarted runner resumes at review", "err", err)
		r.stages = nil
	}
	host, _ := os.Hostname()
	r.publish(control.RegisterSubject(r.opts.Steward, host), Registration{
		Steward: r.opts.Steward, Harnesses: names, Host: host, Pool: r.opts.Pool, Version: r.opts.Version,
	})
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.nc.FlushWithContext(flushCtx); err != nil {
		return fmt.Errorf("register with the control node: %w", err)
	}
	r.log.Info("runner registered", "steward", r.opts.Steward, "harnesses", strings.Join(names, ","), "pool", r.opts.Pool, "nats", r.opts.NATSURL)
	return nil
}

// sharedConsumer opens the durable consumer over name's jobs, which every
// runner for that steward (or every pool runner) shares. Its config is set
// only when this runner creates it: a consumer that already exists is used
// as it is, so a later runner with another --ack-wait does not change the
// lease of jobs other runners are working.
func (r *Runner) sharedConsumer(ctx context.Context, js jetstream.JetStream, name string) (jetstream.Consumer, error) {
	cons, err := js.CreateConsumer(ctx, control.JobStream, jetstream.ConsumerConfig{
		Durable:       control.ConsumerName(name),
		FilterSubject: control.JobFilter(name),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       r.opts.AckWait,
	})
	if errors.Is(err, jetstream.ErrConsumerExists) {
		return js.Consumer(ctx, control.JobStream, control.ConsumerName(name))
	}
	return cons, err
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
		msg, err := r.next()
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

// next returns the steward's own job when one is waiting. A pool job is
// fetched only when that fetch finds nothing and --pool was set. A runner
// without --pool has no pool consumer, so it never receives a pool job.
func (r *Runner) next() (jetstream.Msg, error) {
	msg, err := r.cons.Next(jetstream.FetchMaxWait(pollWait))
	if err == nil || r.poolCons == nil || !errors.Is(err, nats.ErrTimeout) {
		return msg, err
	}
	return r.poolCons.Next(jetstream.FetchMaxWait(pollWait))
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
	// review reports each round it starts.
	if state == StateReviewing && r.stage.PR != "" {
		s := r.stage
		s.Stage, s.Round = stageReview, s.Round+1
		r.mark(job, s)
	}
	r.log.Info("job state", "repo", job.Repo, "issue", job.Issue, "state", string(state), "detail", detail)
	r.publish(statusSubject(job), Status{State: state, Detail: detail, Attempts: r.attempts})
	if r.progress != nil {
		select {
		case r.progress <- struct{}{}:
		default:
		}
	}
}

func statusSubject(job control.Job) string {
	return StatusSubjectPrefix + strings.TrimPrefix(job.Subject(), jobSubjectPrefix)
}

// recordedAttempts returns the failed-work count on the latest persisted
// status for job. Statuses from older runners have no count and decode as zero.
func (r *Runner) recordedAttempts(ctx context.Context, job control.Job) int {
	fallback := r.failures[job.Subject()]
	if r.statuses == nil {
		return fallback
	}
	msg, err := r.statuses.GetLastMsgForSubject(ctx, statusSubject(job))
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return fallback
	}
	var status Status
	if err == nil {
		err = json.Unmarshal(msg.Data, &status)
	}
	if err != nil {
		r.log.Warn("could not restore the job's attempt count", "repo", job.Repo, "issue", job.Issue, "err", err)
		return fallback
	}
	return max(status.Attempts, fallback)
}

func stageKey(job control.Job) string {
	return strings.TrimPrefix(job.Subject(), jobSubjectPrefix)
}

// mark records how far the job got. A record that cannot be written is only
// logged, since GitHub still finds an open pull request.
func (r *Runner) mark(job control.Job, s stage) {
	r.stage = s
	if r.stages == nil {
		return
	}
	data, _ := json.Marshal(s)
	// Not the job's context: a stage reached during shutdown is still kept.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.stages.Put(ctx, stageKey(job), data); err != nil {
		r.log.Warn("could not record the job's stage", "repo", job.Repo, "issue", job.Issue, "stage", s.Stage, "err", err)
	}
}

// recordedStage returns the job's stage record. A missing or unreadable one
// is the zero stage, which starts the job from the beginning.
func (r *Runner) recordedStage(ctx context.Context, job control.Job) stage {
	var s stage
	if r.stages == nil {
		return s
	}
	entry, err := r.stages.Get(ctx, stageKey(job))
	if err == nil {
		err = json.Unmarshal(entry.Value(), &s)
	}
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		r.log.Warn("could not read the job's stage; GitHub decides where it resumes", "repo", job.Repo, "issue", job.Issue, "err", err)
		return stage{}
	}
	return s
}

// token asks the control node for a GitHub token for the job. The control
// node mints a new one on every request while the job is in the stream.
func (r *Runner) token(ctx context.Context, subject string) (control.Token, error) {
	msg, err := r.nc.RequestWithContext(ctx, control.TokenSubjectPrefix+r.opts.Steward, []byte(subject))
	if err != nil {
		return control.Token{}, fmt.Errorf("ask the control node for the job's token: %w", err)
	}
	var reply control.TokenReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return control.Token{}, fmt.Errorf("decode the job's token: %w", err)
	}
	if reply.Error != "" || reply.Token.Token == "" {
		return control.Token{}, fmt.Errorf("the control node could not mint a token: %s", reply.Error)
	}
	return reply.Token, nil
}

// handle works one delivered job. It acknowledges the job only once the job
// has reached a final state, so a runner that dies before then leaves the job
// to be redelivered. A StateFailed outcome is handed back until this process
// has persisted Options.MaxAttempts of them. A hand-back because the token request
// failed, or because the runner's context is canceled, does not count.
func (r *Runner) handle(ctx context.Context, msg jetstream.Msg) {
	var job control.Job
	if err := json.Unmarshal(msg.Data(), &job); err != nil || job.Subject() != msg.Subject() {
		r.log.Error("dropping a malformed job", "subject", msg.Subject(), "err", err)
		_ = msg.Term()
		return
	}

	r.attempts = r.recordedAttempts(ctx, job)
	workCtx, cancelWork := context.WithCancelCause(ctx)
	defer cancelWork(nil)
	progress := make(chan struct{}, 1)
	r.progress = progress
	defer func() { r.progress = nil }()

	// Heartbeats keep the job ours while it is being worked. The watchdog
	// prevents a live but wedged runner from renewing the delivery forever.
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(r.ackWait / 3)
		defer tick.Stop()
		stuck := time.NewTimer(r.opts.StuckAfter)
		defer stuck.Stop()
		for {
			select {
			case <-done:
				return
			case <-progress:
				if !stuck.Stop() {
					select {
					case <-stuck.C:
					default:
					}
				}
				stuck.Reset(r.opts.StuckAfter)
			case <-tick.C:
				_ = msg.InProgress()
			case <-stuck.C:
				r.log.Error("job is stuck; handing it back", "repo", job.Repo, "issue", job.Issue, "stuck_after", r.opts.StuckAfter)
				cancelWork(errJobStuck)
				_ = msg.Nak()
				return
			}
		}
	}()

	r.report(job, StateClaimed, "")
	tokens := &jobToken{fetch: func(ctx context.Context) (control.Token, error) { return r.token(ctx, msg.Subject()) }}
	if _, err := tokens.get(workCtx); err != nil {
		if errors.Is(context.Cause(workCtx), errJobStuck) {
			return
		}
		// Without a token the outcome cannot be put on the issue, so the job
		// is handed back instead of being failed where nobody would see it.
		// This delivery is not a work failure and does not count.
		r.log.Error("handing the job back", "repo", job.Repo, "issue", job.Issue, "err", err)
		_ = msg.NakWithDelay(r.opts.RetryDelay)
		return
	}
	gh := github{forge.GitHub{API: r.opts.GitHubAPIURL, Repo: job.Repo, Tokens: tokens.get}}
	out := r.work(workCtx, gh, job, tokens.get)
	if errors.Is(context.Cause(workCtx), errJobStuck) {
		return
	}
	if ctx.Err() != nil {
		// Shutting down. Hand the job to another runner now, and do not count
		// this delivery as a work failure.
		_ = msg.Nak()
		return
	}
	// A failure may be passing (a harness outage, a GitHub error), so the job
	// is tried again until the persisted count reaches MaxAttempts StateFailed
	// outcomes. The JetStream delivery count is not that number: a token
	// hand-back and a shutdown hand-back are deliveries too. A gate failure
	// is the session's own result and is not retried. A review that does not
	// pass is final too: its rounds are spent. A factory merge that does not
	// land is final as well. So is a failure marked final, such as a missing
	// or invalid repo config, which a retry cannot change.
	if out.state == StateFailed && !out.final {
		r.attempts++
		r.failures[job.Subject()] = r.attempts
		n := r.attempts
		if n < r.opts.MaxAttempts {
			r.report(job, StateRetrying, fmt.Sprintf("attempt %d of %d failed: %s", n, r.opts.MaxAttempts, out.detail))
			if err := r.nc.FlushTimeout(5 * time.Second); err != nil {
				r.log.Warn("could not persist the job's attempt count before handing it back", "repo", job.Repo, "issue", job.Issue, "err", err)
			}
			if err := msg.NakWithDelay(r.opts.RetryDelay); err == nil {
				return
			}
		}
	}
	delete(r.failures, job.Subject())
	if out.comment != "" {
		if err := gh.Comment(ctx, job.Issue, markOutcome(out.comment, out.state)); err != nil {
			r.log.Error("could not comment on the issue", "repo", job.Repo, "issue", job.Issue, "err", err)
		}
	}
	r.report(job, out.state, out.detail)
	if r.stages != nil {
		if err := r.stages.Delete(ctx, stageKey(job)); err != nil {
			r.log.Warn("could not clear the job's stage", "repo", job.Repo, "issue", job.Issue, "err", err)
		}
	}
	if err := msg.Ack(); err != nil {
		r.log.Error("could not acknowledge the job; it will be redelivered", "repo", job.Repo, "issue", job.Issue, "err", err)
	}
}

var errJobStuck = errors.New("job made no progress before stuck-after")

// deadlines bound one job's harness sessions and its gate command.
type deadlines struct {
	session time.Duration
	gate    time.Duration
}

func (r *Runner) deadlinesFor(cfg repoconfig.Config) deadlines {
	return deadlines{
		session: cfg.SessionTimeoutOr(r.opts.SessionTimeout),
		gate:    cfg.GateTimeoutOr(r.opts.GateTimeout),
	}
}

// timeoutError is a session or gate canceled at its deadline. The text is
// what the issue comment reports, and it carries no command output.
type timeoutError struct {
	what string
}

func (e timeoutError) Error() string { return e.what + " timed out" }

// runSession runs one harness session. The deadline is a child context, so
// the harness's process-group kill runs when it fires and a shutdown of the
// parent still cancels the session.
func (r *Runner) runSession(ctx context.Context, limit time.Duration, req harness.Request) error {
	if limit <= 0 {
		limit = repoconfig.DefaultSessionTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := r.harness.Run(ctx, req)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timeoutError{what: "the " + r.harness.Name() + " session"}
	}
	return err
}

// runGate runs the gate command. Workspace.Run puts it in its own process
// group and kills that group when ctx is canceled. A timeout drops the
// command output so a comment cannot echo it.
func (r *Runner) runGate(ctx context.Context, ws Workspace, limit time.Duration, command string) (string, error) {
	if limit <= 0 {
		limit = repoconfig.DefaultGateTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	output, err := ws.Run(ctx, command)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", timeoutError{what: "the gate"}
	}
	return output, err
}

// finishSessionErr keeps a timeout's own text and wraps every other session
// failure the way the runner always has.
func finishSessionErr(kind string, err error) error {
	if err == nil {
		return nil
	}
	var timedOut timeoutError
	if errors.As(err, &timedOut) {
		return err
	}
	return fmt.Errorf("the %s session did not finish: %w", kind, err)
}

// outcome is the final state of one attempt at a job.
type outcome struct {
	state  State
	detail string
	// comment is posted on the issue. Empty posts nothing.
	comment string
	// final keeps a StateFailed outcome from being retried.
	final bool
}

func failed(err error) outcome {
	return outcome{
		state:   StateFailed,
		detail:  err.Error(),
		comment: "schmeide could not finish this job and did not open a pull request:\n\n" + err.Error(),
	}
}

// finalFailed is a failure that a retry cannot change, so it is not retried.
func finalFailed(err error) outcome {
	out := failed(err)
	out.final = true
	return out
}

// work takes the job as far as it goes on this attempt.
func (r *Runner) work(ctx context.Context, gh forge.Forge, job control.Job, token func(context.Context) (string, error)) outcome {
	if job.Title == "" {
		return failed(errors.New("the job does not carry the issue's text; remove the ready label and add it again to make a new job"))
	}
	origin := strings.TrimSuffix(r.opts.GitHubURL, "/") + "/" + job.Repo + ".git"
	if err := trust.Check(r.opts.WorkDir, origin, r.opts.Confirm); err != nil {
		return failed(err)
	}
	branch := BranchName(job.Issue, job.Title)
	// A redelivered job whose first runner died after opening the PR must not
	// open a second one. The guard goes by the issue number, not the whole
	// branch name, which changes when the issue is retitled. A stacked pull
	// request matches here too, and a later review pushes the same head.
	pr, found, err := gh.IssuePull(ctx, job.Issue, "all")
	if err != nil {
		return failed(err)
	}
	r.stage = r.recordedStage(ctx, job)
	if r.stage.PR == "" {
		r.stage = stage{}
	}
	if !found && r.stage.PR != "" {
		// GitHub can leave a just-opened pull request out of its list. The
		// stage record still names it, so no second one is opened.
		pr = forge.Pull{HTMLURL: r.stage.PR, State: "open", CreatedAt: r.stage.Opened}
		pr.Head.Ref, pr.Base.Ref = r.stage.Branch, r.stage.Base
		found = true
	}
	var base string
	if found {
		// An outcome posted since the pull request opened means only the
		// acknowledgment was lost. It is not posted again.
		state, posted, err := postedOutcome(ctx, gh, job.Issue, pr.CreatedAt)
		if err != nil {
			return outcome{state: StateFailed, detail: err.Error(), comment: "schmeide could not finish this job and left the pull request open: " + pr.HTMLURL + "\n\n" + err.Error()}
		}
		if posted {
			return outcome{state: state, detail: pr.HTMLURL}
		}
		// A closed pull request is someone's decision, and the first attempt's
		// pull request already links the issue.
		if pr.State != "open" {
			return outcome{state: StatePROpened, detail: pr.HTMLURL}
		}
		branch, base = pr.Head.Ref, pr.Base.Ref
		if r.stage.PR == "" {
			r.stage = stage{Stage: stagePROpened, Branch: branch, Base: base, PR: pr.HTMLURL, Opened: pr.CreatedAt}
		}
	} else if base, err = baseBranch(ctx, gh, job.Issue); err != nil {
		// Asked before the worktree exists, so a stacked job starts on the
		// dependency's branch and the pull request opens against it.
		if errors.Is(err, errNotOneStack) {
			return finalFailed(err)
		}
		return failed(err)
	}

	ws, err := newWorktree(ctx, worktreeSpec{
		workDir:  r.opts.WorkDir,
		url:      origin,
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
	if !found {
		r.mark(job, stage{Stage: stageWorktreeReady, Branch: branch, Base: base})
	}
	return r.execute(ctx, ws, gh, job, branch, base, pr.HTMLURL)
}

// execute is the job itself, written against Workspace only. A non-empty
// prURL is the job's open pull request from an earlier delivery, which is
// resumed at its review.
func (r *Runner) execute(ctx context.Context, ws Workspace, gh forge.Forge, job control.Job, branch, base, prURL string) outcome {
	// The gate is read before the session runs, so the session cannot weaken it.
	cfg, err := repoconfig.Load(ws.Dir())
	if err != nil {
		// The config is read from the base branch, which a retry does not
		// change.
		return finalFailed(err)
	}
	// The gate string is the one just read. Detection runs here, before the
	// session, and not during git commit or git push.
	r.warnLintGate(ctx, gh, job, ws.Dir(), cfg.Gate)
	limits := r.deadlinesFor(cfg)
	url, spent := prURL, 0
	if url == "" {
		out, ok := r.open(ctx, ws, gh, job, cfg.Gate, branch, base, limits)
		if !ok {
			return out
		}
		url = out.detail
	} else {
		// The branch moves to the pull request's head only now, after the
		// gate was read from the base.
		if err := ws.Resume(ctx); err != nil {
			return reviewFailed(url, err)
		}
		// ponytail: a rejection whose fix was never pushed is not counted, so
		// a redelivery can run one round more than --max-review-rounds.
		if spent, err = ws.Count(ctx, fixMessage(job.Issue)); err != nil {
			return reviewFailed(url, err)
		}
		// A started round that is recorded but whose fix never reached the
		// branch is run again.
		spent = max(spent, r.stage.Round-1)
	}
	r.stage.Round = spent
	out := r.review(ctx, ws, job, cfg.Gate, url, spent, cfg.Automate.ReviewFix, limits)
	// The automate switches were read with the gate, before the session ran,
	// so the session cannot turn factory merge on for itself. Off leaves the
	// pull request for a person.
	if !cfg.Automate.Merge || out.state != StatePROpened {
		return out
	}
	// One fix round for a failed check, when a review round is left for it.
	var fix ciFix
	if r.stage.Round < r.opts.MaxReviewRounds {
		fix = func(ctx context.Context, run ciwatch.Run, tail string) (outcome, bool) {
			return r.fixCheck(ctx, ws, job, cfg.Gate, url, limits, run, tail)
		}
	}
	return r.mergeWhenGreen(ctx, ws, gh, job.Issue, base, url, cfg.CheckWait, fix)
}

// warnLintGate comments when the configured gate does not mention a lint
// target detected in the checkout. The comment is a warning: the job still
// runs. A detection or comment error is logged and does not fail the job.
func (r *Runner) warnLintGate(ctx context.Context, gh forge.Forge, job control.Job, root, gate string) {
	found, err := lintgate.Detect(root)
	if err != nil {
		r.log.Warn("could not detect a repo lint gate", "repo", job.Repo, "issue", job.Issue, "err", err)
		return
	}
	body := lintgate.Warning(gate, found)
	if body == "" {
		return
	}
	if err := gh.Comment(ctx, job.Issue, body); err != nil {
		r.log.Error("could not comment on the detected lint gate", "repo", job.Repo, "issue", job.Issue, "err", err)
	}
}

// open runs the session, the gate, and the push, and opens the pull request.
// ok is true when out.detail is the new pull request's URL.
func (r *Runner) open(ctx context.Context, ws Workspace, gh forge.Forge, job control.Job, gate, branch, base string, limits deadlines) (out outcome, ok bool) {
	r.report(job, StateWorking, r.harness.Name())
	if err := finishSessionErr(r.harness.Name(), r.runSession(ctx, limits.session, r.session(ws.Dir(), brief(job, gate)))); err != nil {
		return failed(err), false
	}
	r.mark(job, stage{Stage: stageSessionFinished, Branch: branch, Base: base})
	title, body := prText(ws.Dir(), job.Issue, job.Title)
	changed, err := ws.Commit(ctx, title)
	if err != nil {
		return failed(err), false
	}
	if !changed {
		return failed(fmt.Errorf("the %s session made no changes", r.harness.Name())), false
	}
	if output, err := r.runGate(ctx, ws, limits.gate, gate); err != nil {
		return r.gateOutcome(job, gate, "", err, output), false
	}
	r.mark(job, stage{Stage: stageGatePassed, Branch: branch, Base: base})
	if err := ws.Push(ctx); err != nil {
		return failed(err), false
	}
	opened := time.Now().UTC().Format(time.RFC3339)
	url, err := gh.OpenPull(ctx, title, body, branch, base)
	if err != nil {
		return failed(err), false
	}
	r.mark(job, stage{Stage: stagePROpened, Branch: branch, Base: base, PR: url, Opened: opened})
	return outcome{detail: url}, true
}

// gateOutcome reports a failed gate. prURL is empty when no pull request was
// opened. The gate's output stays in the runner log: the session wrote the
// code that produced it, so it is not trusted to be safe to publish.
func (r *Runner) gateOutcome(job control.Job, gate, prURL string, runErr error, output string) outcome {
	var timedOut timeoutError
	if errors.As(runErr, &timedOut) {
		detail := timedOut.Error()
		r.log.Warn("gate timed out", "repo", job.Repo, "issue", job.Issue)
		comment := "schmeide did not open a pull request: " + detail + "."
		if prURL != "" {
			comment = "schmeide left the pull request open: " + detail + ". " + prURL
		}
		return outcome{state: StateGateFailed, detail: detail, comment: comment}
	}
	if len(output) > gateTailBytes {
		output = "...\n" + output[len(output)-gateTailBytes:]
	}
	detail := fmt.Sprintf("the gate command `%s` failed: %v", gate, runErr)
	r.log.Warn("gate failed", "repo", job.Repo, "issue", job.Issue, "err", runErr, "output", output)
	comment := "schmeide did not open a pull request: " + detail + ". The gate's output is in the runner's log."
	if prURL != "" {
		comment = "schmeide left the pull request open: " + detail + ". The gate's output is in the runner's log. " + prURL
	}
	return outcome{state: StateGateFailed, detail: detail, comment: comment}
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
func brief(job control.Job, gate string) string {
	return fmt.Sprintf(`You are working on GitHub issue #%[1]d of %[2]s, in a fresh checkout of the repository on its own branch.

Issue title: %[3]s

Issue body:
%[4]s

Make the change the issue asks for, with tests where the repository expects them. Follow the repository's own conventions (README, CONTRIBUTING, agent instruction files). Stay inside this directory. Do not push and do not open a pull request: that is done for you once the repository's gate command passes. The gate command is: %[5]s

When you are done, write the pull request text to %[6]s: the title on the first line, following the repository's commit and pull request title conventions, then a blank line, then the body, following the repository's pull request template if it has one. The body must contain the line "Closes #%[1]d".
`, job.Issue, job.Repo, job.Title, job.Body, gate, prTextPath)
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
