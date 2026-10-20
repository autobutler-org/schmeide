package control

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/autobutler-org/schmeide/internal/release"
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

	// StatusStream keeps runner registrations and job statuses in the queue's
	// data directory. It is the history a restarted control node or runner
	// loads.
	StatusStream = "SCHMEIDE_STATUS"
)

// State is where a job stands. Gate failed, PR opened, failed, review failed,
// merged, queued, and merge failed are final; retrying means an attempt failed
// and the job will be delivered again, and reviewing is still in progress.
type State string

// The states a job's status can hold. The control node reports pending when
// it creates a job; runners report the rest.
const (
	StatePending    State = "pending"
	StateClaimed    State = "claimed"
	StateWorking    State = "working"
	StateRetrying   State = "retrying"
	StateGateFailed State = "gate_failed"
	StatePROpened   State = "pr_opened"
	StateFailed     State = "failed"

	StateReviewing    State = "reviewing"
	StateReviewFailed State = "review_failed"
	StateMerged       State = "merged"
	StateQueued       State = "queued"
	StateMergeFailed  State = "merge_failed"
)

// final reports whether a job in this state is done. Pending is not.
func (s State) final() bool {
	switch s {
	case StatePROpened, StateReviewFailed, StateMerged, StateQueued, StateMergeFailed:
		return true
	}
	return s.failure()
}

// failure reports whether a job ended without a pull request. Its issue may
// get a new job.
func (s State) failure() bool { return s == StateGateFailed || s == StateFailed }

// Status is the message a runner publishes when a job changes state, and the
// one the control node publishes with StatePending when it creates a job.
type Status struct {
	State State `json:"state"`
	// Detail is the PR URL for StatePROpened and the reason for the failure
	// states.
	Detail string `json:"detail,omitempty"`
	// Attempts is the number of failed work outcomes so far. It is omitted
	// for zero so statuses written by older runners remain compatible.
	Attempts int `json:"attempts,omitempty"`
}

// Registration is the message a runner publishes when it starts.
type Registration struct {
	Steward   string   `json:"steward"`
	Harnesses []string `json:"harnesses"`
	Host      string   `json:"host"`
	// Pool is true only when the runner was started with --pool. Such a
	// runner may take a job whose steward has no registration.
	Pool bool `json:"pool,omitempty"`
	// Version is the runner's release.Version.
	Version string `json:"version,omitempty"`
}

// Tracker records what runners report: who registered and where each job
// stands. Registrations and statuses are stored on StatusStream, and Track
// loads that history before it returns.
type Tracker struct {
	stream  jetstream.Stream
	issues  jetstream.KeyValue
	log     *slog.Logger
	mu      sync.Mutex
	runners []Registration
	jobs    map[string]Status
	// since is when each job's latest status was stored.
	since map[string]time.Time
}

// Track records runner registrations and job statuses. Messages already
// stored in the queue's data directory are applied before Track returns;
// messages published after that are applied as they arrive.
func (q *Queue) Track(log *slog.Logger) (*Tracker, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	// The same subjects runners already publish. JetStream stores each
	// message because the stream listens on them; publishers stay unchanged.
	stream, err := q.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: StatusStream,
		Subjects: []string{
			RegisterSubjectPrefix + "*",
			StatusSubjectPrefix + "*.*.*",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create status stream: %w", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("read status stream: %w", err)
	}
	t := &Tracker{stream: stream, issues: q.issues, log: log, jobs: map[string]Status{}, since: map[string]time.Time{}}
	if err := t.replay(ctx, stream, log, info.State); err != nil {
		return nil, err
	}
	// start is the first sequence replay did not apply. A message stored
	// while replaying is still delivered, and nothing already applied is
	// delivered again.
	if err := t.follow(ctx, stream, log, info.State.LastSeq+1); err != nil {
		return nil, err
	}
	// Publish consults this. It is set only after replay, so a restart
	// already knows which stewards have a runner.
	q.tracker = t
	return t, nil
}

// replay applies the status stream in order. A malformed registration or a
// junk status is dropped here the same way it is dropped live, so a restart
// does not bring it back.
func (t *Tracker) replay(ctx context.Context, stream jetstream.Stream, log *slog.Logger, state jetstream.StreamState) error {
	if state.Msgs == 0 {
		return nil
	}
	for seq := state.FirstSeq; seq <= state.LastSeq; seq++ {
		msg, err := stream.GetMsg(ctx, seq)
		if err != nil {
			return fmt.Errorf("load status history: %w", err)
		}
		t.observe(log, msg.Sequence, msg.Time, msg.Subject, msg.Data)
	}
	return nil
}

// follow delivers messages stored at start and after.
func (t *Tracker) follow(ctx context.Context, stream jetstream.Stream, log *slog.Logger, start uint64) error {
	cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{OptStartSeq: start})
	if err != nil {
		return fmt.Errorf("follow status stream: %w", err)
	}
	_, err = cons.Consume(func(msg jetstream.Msg) {
		meta, err := msg.Metadata()
		if err != nil {
			log.Error("dropping a status without metadata", "subject", msg.Subject(), "err", err)
			return
		}
		t.observe(log, meta.Sequence.Stream, meta.Timestamp, msg.Subject(), msg.Data())
	})
	if err != nil {
		return fmt.Errorf("follow status stream: %w", err)
	}
	return nil
}

func (t *Tracker) observe(log *slog.Logger, seq uint64, at time.Time, subject string, data []byte) {
	switch {
	case strings.HasPrefix(subject, RegisterSubjectPrefix):
		t.observeRegistration(log, subject, data)
	case strings.HasPrefix(subject, StatusSubjectPrefix):
		t.observeStatus(log, seq, at, subject, data)
	}
}

func (t *Tracker) observeRegistration(log *slog.Logger, subject string, data []byte) {
	var reg Registration
	// The subject, not the payload, is what NATS permissions can pin to a
	// steward, so the two must agree.
	if err := json.Unmarshal(data, &reg); err != nil || RegisterSubjectPrefix+reg.Steward != subject {
		log.Warn("dropping a malformed runner registration", "subject", subject, "err", err)
		return
	}
	log.Info("runner registered", "steward", reg.Steward, "host", reg.Host, "harnesses", strings.Join(reg.Harnesses, ","), "pool", reg.Pool, "version", reg.Version)
	// A skewed runner is still accepted; the warning is for the operator.
	if release.Skewed(reg.Version, release.Version) {
		log.Warn("runner version differs from the control node; upgrade one so they match",
			"steward", reg.Steward, "host", reg.Host, "runner_version", reg.Version, "control_version", release.Version)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// A restarted runner registers again; keep one entry per steward and host.
	t.runners = slices.DeleteFunc(t.runners, func(r Registration) bool {
		return r.Steward == reg.Steward && r.Host == reg.Host
	})
	t.runners = append(t.runners, reg)
}

func (t *Tracker) observeStatus(log *slog.Logger, seq uint64, at time.Time, subject string, data []byte) {
	var status Status
	if err := json.Unmarshal(data, &status); err != nil || status.State == "" {
		log.Warn("dropping a malformed job status", "subject", subject, "err", err)
		return
	}
	job := jobSubjectPrefix + strings.TrimPrefix(subject, StatusSubjectPrefix)
	log.Info("job state", "job", job, "state", string(status.State), "detail", status.Detail)
	t.mu.Lock()
	t.jobs[job] = status
	t.since[job] = at
	t.mu.Unlock()
	if status.State.final() {
		if err := t.finish(seq, job, status.State); err != nil {
			log.Error("could not record the job's final state", "job", job, "state", string(status.State), "err", err)
		}
	}
}

// finish records the final state on the job's issue record, unless the
// record now belongs to a later job: one on another subject, or one published
// after this status was stored.
func (t *Tracker) finish(seq uint64, job string, state State) error {
	if t.issues == nil {
		// ReadStatus only reads; the control node records final states.
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	parts := strings.Split(job, ".")
	key := strings.Join(parts[len(parts)-2:], ".")
	entry, err := t.issues.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var record issueRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		return err
	}
	if record.Subject != job || record.State != "" || seq <= record.After {
		return nil
	}
	record.State = state
	data, _ := json.Marshal(record)
	_, err = t.issues.Update(ctx, key, data, entry.Revision())
	return err
}

// lastSeq is the last sequence stored on the status stream.
func (t *Tracker) lastSeq(ctx context.Context) (uint64, error) {
	info, err := t.stream.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("read status stream: %w", err)
	}
	return info.State.LastSeq, nil
}

// hasRunner reports whether the steward has at least one registration.
// Registrations are not expired: a runner that stops stays registered.
func (t *Tracker) hasRunner(steward string) bool {
	return t.registered(steward, false)
}

// poolRunner reports whether the steward has a registration with the pool flag.
func (t *Tracker) poolRunner(steward string) bool {
	return t.registered(steward, true)
}

func (t *Tracker) registered(steward string, poolOnly bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.runners {
		if r.Steward == steward && (!poolOnly || r.Pool) {
			return true
		}
	}
	return false
}

// Runners returns the registered runners, oldest registration first.
func (t *Tracker) Runners() []Registration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.runners)
}

// JobStatus returns the latest status reported for the job on the given
// subject (see Job.Subject), and whether any was.
func (t *Tracker) JobStatus(subject string) (Status, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	status, ok := t.jobs[subject]
	return status, ok
}

// JobState is one unfinished job in a Snapshot: pending until a runner
// reports on it, then the runner's latest state.
type JobState struct {
	// Subject is the job's subject (see Job.Subject).
	Subject string `json:"subject"`
	State   State  `json:"state"`
	Detail  string `json:"detail,omitempty"`
	// Since is when the latest status was stored: for a pending job, when
	// it was queued.
	Since time.Time `json:"since"`
}

// Snapshot is what `schmeide status` reports. Its JSON form is stable.
type Snapshot struct {
	Runners []Registration `json:"runners"`
	// Jobs are the jobs whose latest state is not final, by subject.
	Jobs []JobState `json:"jobs"`
}

// ReadStatus replays the status stream into a Snapshot. It only reads: no
// final state is recorded on an issue. A missing stream is an empty Snapshot.
func ReadStatus(ctx context.Context, js jetstream.JetStream) (Snapshot, error) {
	snap := Snapshot{Runners: []Registration{}, Jobs: []JobState{}}
	stream, err := js.Stream(ctx, StatusStream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return snap, nil
	}
	if err != nil {
		return snap, fmt.Errorf("read status stream: %w", err)
	}
	t := &Tracker{jobs: map[string]Status{}, since: map[string]time.Time{}}
	if err := t.replay(ctx, stream, slog.New(slog.DiscardHandler), stream.CachedInfo().State); err != nil {
		return snap, err
	}
	snap.Runners = append(snap.Runners, t.runners...)
	for subject, status := range t.jobs {
		if !status.State.final() {
			snap.Jobs = append(snap.Jobs, JobState{Subject: subject, State: status.State, Detail: status.Detail, Since: t.since[subject]})
		}
	}
	slices.SortFunc(snap.Jobs, func(a, b JobState) int { return strings.Compare(a.Subject, b.Subject) })
	return snap, nil
}

// WriteText writes the Snapshot as aligned columns.
func (s Snapshot) WriteText(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(s.Runners) == 0 {
		fmt.Fprintln(tw, "No registered runners.")
	} else {
		fmt.Fprintln(tw, "RUNNER\tHOST\tHARNESSES\tPOOL")
		for _, r := range s.Runners {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%t\n", r.Steward, r.Host, strings.Join(r.Harnesses, ","), r.Pool)
		}
	}
	tw.Flush()
	fmt.Fprintln(w)
	if len(s.Jobs) == 0 {
		fmt.Fprintln(w, "No queued or running jobs.")
		return
	}
	fmt.Fprintln(tw, "JOB\tSTATE\tFOR\tDETAIL")
	for _, j := range s.Jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.Subject, j.State, time.Since(j.Since).Round(time.Second), j.Detail)
	}
	tw.Flush()
}

// StatusUsage is the help text of `schmeide status`.
func StatusUsage() string {
	return `schmeide status shows the runners registered with the control node and the
jobs that are queued or running, read from the control node's status stream.
A job no runner has claimed yet is "pending"; FOR is how long a job has been
in its state, so for a pending job it is how long the job has waited.
It connects to the control node's NATS as a steward, the same way a runner does.
Status is team-wide and read-only: any steward sees every steward's runners and
jobs, pool jobs included, and running it changes nothing.

Usage:
  schmeide status --steward <github login> [--json] [flags]

Flags:
  --steward
        Your GitHub login. Required.
        Env: SCHMEIDE_STEWARD
  --nats-url
        NATS URL of the control node, which is its --nats-addr.
        Env: SCHMEIDE_NATS_URL (default "nats://127.0.0.1:4222")
  --nats-token
        This steward's NATS token: the one the control node's --runner-tokens lists for --steward.
        Env: SCHMEIDE_NATS_TOKEN
  --json
        Print one JSON object with "runners" and "jobs" instead of text. Each
        job's "since" is when it entered its state.
  -h, --help
        Show this help.
`
}

// StatusMain runs `schmeide status` and returns its exit code.
func StatusMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schmeide status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	steward := fs.String("steward", env("SCHMEIDE_STEWARD", ""), "")
	url := fs.String("nats-url", env("SCHMEIDE_NATS_URL", "nats://127.0.0.1:4222"), "")
	token := fs.String("nats-token", env("SCHMEIDE_NATS_TOKEN", ""), "")
	asJSON := fs.Bool("json", false, "")
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, StatusUsage())
		return 0
	}
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err == nil && *steward == "" {
		err = errors.New("missing --steward: pass your GitHub login with --steward or set SCHMEIDE_STEWARD")
	}
	if err != nil {
		fmt.Fprintf(stderr, "schmeide status: %v\nRun 'schmeide status --help' for usage.\n", err)
		return 2
	}
	nc, err := nats.Connect(*url, nats.UserInfo(*steward, *token), nats.CustomInboxPrefix(InboxPrefix(*steward)), nats.Name("schmeide status"))
	if err != nil {
		fmt.Fprintf(stderr, "schmeide status: cannot reach the control node's NATS at %s as %s: %v\n"+
			"Check that `schmeide control` is running, that --nats-url points at its --nats-addr, "+
			"and that --nats-token is the token its --runner-tokens lists for %s.\n", *url, *steward, err, *steward)
		return 1
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		fmt.Fprintf(stderr, "schmeide status: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	snap, err := ReadStatus(ctx, js)
	if err != nil {
		fmt.Fprintf(stderr, "schmeide status: %v\nCheck that --nats-url points at a `schmeide control` node.\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
		return 0
	}
	snap.WriteText(stdout)
	return 0
}
