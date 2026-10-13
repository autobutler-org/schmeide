package control

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nats-io/nats.go"
)

const (
	// RegisterSubjectPrefix plus a steward is the subject a runner announces
	// itself on when it starts. The message is a Registration.
	RegisterSubjectPrefix = "schmeide.runners."
	// StatusSubjectPrefix replaces "schmeide.jobs." in a job's subject to give
	// the subject its Status messages are published on.
	StatusSubjectPrefix = "schmeide.status."
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

// Tracker records what runners report: who registered and where each job
// stands.
//
// ponytail: in memory only, so a control node restart forgets it all; put
// status on a JetStream stream when something needs the history.
type Tracker struct {
	mu      sync.Mutex
	runners []Registration
	jobs    map[string]Status
}

// Track subscribes to runner registrations and job statuses.
func (q *Queue) Track(log *slog.Logger) (*Tracker, error) {
	t := &Tracker{jobs: map[string]Status{}}
	if _, err := q.nc.Subscribe(RegisterSubjectPrefix+"*", func(m *nats.Msg) {
		var reg Registration
		// The subject, not the payload, is what NATS permissions can pin to a
		// steward, so the two must agree.
		if err := json.Unmarshal(m.Data, &reg); err != nil || RegisterSubjectPrefix+reg.Steward != m.Subject {
			log.Warn("dropping a malformed runner registration", "subject", m.Subject, "err", err)
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
	}); err != nil {
		return nil, err
	}
	if _, err := q.nc.Subscribe(StatusSubjectPrefix+"*.*.*", func(m *nats.Msg) {
		var status Status
		if err := json.Unmarshal(m.Data, &status); err != nil || status.State == "" {
			log.Warn("dropping a malformed job status", "subject", m.Subject, "err", err)
			return
		}
		job := jobSubjectPrefix + strings.TrimPrefix(m.Subject, StatusSubjectPrefix)
		log.Info("job state", "job", job, "state", string(status.State), "detail", status.Detail)
		t.mu.Lock()
		defer t.mu.Unlock()
		t.jobs[job] = status
	}); err != nil {
		return nil, err
	}
	return t, nil
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
