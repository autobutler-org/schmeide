package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// RegisterSubjectPrefix plus a steward is the subject a runner announces
	// itself on when it starts. The message is a Registration.
	RegisterSubjectPrefix = "schmeide.runners."
	// StatusSubjectPrefix replaces "schmeide.jobs." in a job's subject to give
	// the subject its Status messages are published on.
	StatusSubjectPrefix = "schmeide.status."

	// statusStream keeps runner registrations and job statuses in the queue's
	// data directory. It is the history a restarted control node loads.
	statusStream = "SCHMEIDE_STATUS"
)

// State is where a job stands. Gate failed, PR opened, and failed are final;
// retrying means an attempt failed and the job will be delivered again.
type State string

// The states a runner reports for a job.
const (
	StateClaimed    State = "claimed"
	StateWorking    State = "working"
	StateRetrying   State = "retrying"
	StateGateFailed State = "gate_failed"
	StatePROpened   State = "pr_opened"
	StateFailed     State = "failed"
)

// final reports whether a job in this state is done.
func (s State) final() bool {
	return s == StateGateFailed || s == StatePROpened || s.failure()
}

// failure reports whether a job ended without a pull request. Its issue may
// get a new job.
func (s State) failure() bool { return s == StateGateFailed || s == StateFailed }

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
	// Pool is true only when the runner was started with --pool. Such a
	// runner may take a job whose steward has no registration.
	Pool bool `json:"pool,omitempty"`
}

// Tracker records what runners report: who registered and where each job
// stands. Registrations and statuses are stored on statusStream, and Track
// loads that history before it returns.
type Tracker struct {
	stream  jetstream.Stream
	issues  jetstream.KeyValue
	mu      sync.Mutex
	runners []Registration
	jobs    map[string]Status
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
		Name: statusStream,
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
	t := &Tracker{stream: stream, issues: q.issues, jobs: map[string]Status{}}
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
		t.observe(log, msg.Sequence, msg.Subject, msg.Data)
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
		t.observe(log, meta.Sequence.Stream, msg.Subject(), msg.Data())
	})
	if err != nil {
		return fmt.Errorf("follow status stream: %w", err)
	}
	return nil
}

func (t *Tracker) observe(log *slog.Logger, seq uint64, subject string, data []byte) {
	switch {
	case strings.HasPrefix(subject, RegisterSubjectPrefix):
		t.observeRegistration(log, subject, data)
	case strings.HasPrefix(subject, StatusSubjectPrefix):
		t.observeStatus(log, seq, subject, data)
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
	log.Info("runner registered", "steward", reg.Steward, "host", reg.Host, "harnesses", strings.Join(reg.Harnesses, ","), "pool", reg.Pool)
	t.mu.Lock()
	defer t.mu.Unlock()
	// A restarted runner registers again; keep one entry per steward and host.
	t.runners = slices.DeleteFunc(t.runners, func(r Registration) bool {
		return r.Steward == reg.Steward && r.Host == reg.Host
	})
	t.runners = append(t.runners, reg)
}

func (t *Tracker) observeStatus(log *slog.Logger, seq uint64, subject string, data []byte) {
	var status Status
	if err := json.Unmarshal(data, &status); err != nil || status.State == "" {
		log.Warn("dropping a malformed job status", "subject", subject, "err", err)
		return
	}
	job := jobSubjectPrefix + strings.TrimPrefix(subject, StatusSubjectPrefix)
	log.Info("job state", "job", job, "state", string(status.State), "detail", status.Detail)
	t.mu.Lock()
	t.jobs[job] = status
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
