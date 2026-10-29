package dashboard

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/release"
)

// maxActivity is how many activity events the board keeps, newest first.
const maxActivity = 200

// Job is one job on the dashboard: its control.JobState plus what its subject
// says about it.
type Job struct {
	control.JobState

	// RepoID and Issue come from the subject's last two tokens.
	RepoID string `json:"repo_id"`
	Issue  int    `json:"issue"`
	// Pool is true for a job on the pool subject.
	Pool bool `json:"pool,omitempty"`
}

// Runner is a live registration, and whether its version differs from this
// binary's (see release.Skewed).
type Runner struct {
	control.Registration

	Skewed bool `json:"skewed,omitempty"`
}

// Event is one line of the activity log: a job seen in a state it had not
// been seen in before. At is when that state was stored on the control node,
// not when the dashboard noticed it.
type Event struct {
	At     time.Time     `json:"at"`
	Job    string        `json:"subject"`
	Repo   string        `json:"repo,omitempty"`
	Title  string        `json:"title,omitempty"`
	Issue  int           `json:"issue"`
	State  control.State `json:"state"`
	Detail string        `json:"detail,omitempty"`

	key string
}

// View is what GET /api/state returns and what the ask panel's prompt
// describes.
type View struct {
	Steward string `json:"steward"`
	// Version is this binary's release.Version.
	Version string `json:"version"`
	// Model is the claude model the ask panel uses.
	Model string `json:"model"`
	// Connected is false when the latest status query failed; Error then
	// says why, and the rest is what the last good query returned.
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	// Updated is when the last good query returned. It is zero before one
	// has.
	Updated  time.Time `json:"updated"`
	Runners  []Runner  `json:"runners"`
	Jobs     []Job     `json:"jobs"`
	Finished []Job     `json:"finished"`
	Activity []Event   `json:"activity"`
}

// board keeps the latest View. The poller writes it and the HTTP handlers
// read it.
type board struct {
	mu   sync.Mutex
	view View
	// seen holds the key of every event in view.Activity.
	seen map[string]bool
}

func newBoard(steward, model string) *board {
	return &board{
		view: View{Steward: steward, Version: release.Version, Model: model, Runners: []Runner{}, Jobs: []Job{}, Finished: []Job{}, Activity: []Event{}},
		seen: map[string]bool{},
	}
}

// View returns a copy of the current view.
func (b *board) View() View {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.view
	v.Runners = slices.Clone(v.Runners)
	v.Jobs = slices.Clone(v.Jobs)
	v.Finished = slices.Clone(v.Finished)
	v.Activity = slices.Clone(v.Activity)
	return v
}

// update applies the result of one status query made at now. A failed
// query keeps the last good data and marks the view disconnected.
func (b *board) update(snap control.Snapshot, err error, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.view.Connected, b.view.Error = false, err.Error()
		return
	}
	b.view.Connected, b.view.Error, b.view.Updated = true, "", now
	b.view.Runners = []Runner{}
	for _, r := range snap.Runners {
		b.view.Runners = append(b.view.Runners, Runner{Registration: r, Skewed: release.Skewed(r.Version, release.Version)})
	}
	b.view.Jobs = jobs(snap.Jobs)
	b.view.Finished = jobs(snap.Finished)
	b.record(b.view.Jobs)
	b.record(b.view.Finished)
}

// record adds an event for each job whose state has not been seen yet, then
// keeps the newest maxActivity. The caller holds b.mu.
func (b *board) record(js []Job) {
	for _, j := range js {
		key := j.Subject + "|" + string(j.State) + "|" + strconv.FormatInt(j.Since.UnixNano(), 10)
		if b.seen[key] {
			continue
		}
		b.seen[key] = true
		b.view.Activity = append(b.view.Activity, Event{
			At: j.Since, Job: j.Subject, Repo: j.Repo, Title: j.Title, Issue: j.Issue,
			State: j.State, Detail: j.Detail, key: key,
		})
	}
	slices.SortStableFunc(b.view.Activity, func(x, y Event) int { return y.At.Compare(x.At) })
	if len(b.view.Activity) > maxActivity {
		for _, e := range b.view.Activity[maxActivity:] {
			delete(b.seen, e.key)
		}
		b.view.Activity = b.view.Activity[:maxActivity]
	}
}

// jobs turns job states into Jobs. A subject that is not
// schmeide.jobs.<steward>.<repo id>.<issue> keeps zero RepoID and Issue.
func jobs(states []control.JobState) []Job {
	out := []Job{}
	for _, s := range states {
		j := Job{JobState: s}
		parts := strings.Split(s.Subject, ".")
		if len(parts) == 5 {
			j.Pool = parts[2] == control.PoolName
			j.RepoID = parts[3]
			j.Issue, _ = strconv.Atoi(parts[4])
		}
		out = append(out, j)
	}
	return out
}
