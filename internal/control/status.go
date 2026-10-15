package control

import (
	"context"
	"encoding/json"
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

// Tracker records what runners report: who registered and where each job
// stands. Registrations and statuses are stored on statusStream, and Track
// loads that history before it returns.
type Tracker struct {
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
	t := &Tracker{jobs: map[string]Status{}}
	if err := t.replay(ctx, stream, log, info.State); err != nil {
		return nil, err
	}
	// start is the first sequence replay did not apply. A message stored
	// while replaying is still delivered, and nothing already applied is
	// delivered again.
	if err := t.follow(ctx, stream, log, info.State.LastSeq+1); err != nil {
		return nil, err
	}
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
		t.observe(log, msg.Subject, msg.Data)
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
		t.observe(log, msg.Subject(), msg.Data())
	})
	if err != nil {
		return fmt.Errorf("follow status stream: %w", err)
	}
	return nil
}

func (t *Tracker) observe(log *slog.Logger, subject string, data []byte) {
	switch {
	case strings.HasPrefix(subject, RegisterSubjectPrefix):
		t.observeRegistration(log, subject, data)
	case strings.HasPrefix(subject, StatusSubjectPrefix):
		t.observeStatus(log, subject, data)
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
	log.Info("runner registered", "steward", reg.Steward, "host", reg.Host, "harnesses", strings.Join(reg.Harnesses, ","))
	t.mu.Lock()
	defer t.mu.Unlock()
	// A restarted runner registers again; keep one entry per steward and host.
	t.runners = slices.DeleteFunc(t.runners, func(r Registration) bool {
		return r.Steward == reg.Steward && r.Host == reg.Host
	})
	t.runners = append(t.runners, reg)
}

func (t *Tracker) observeStatus(log *slog.Logger, subject string, data []byte) {
	var status Status
	if err := json.Unmarshal(data, &status); err != nil || status.State == "" {
		log.Warn("dropping a malformed job status", "subject", subject, "err", err)
		return
	}
	job := jobSubjectPrefix + strings.TrimPrefix(subject, StatusSubjectPrefix)
	log.Info("job state", "job", job, "state", string(status.State), "detail", status.Detail)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jobs[job] = status
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
