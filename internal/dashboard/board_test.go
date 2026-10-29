package dashboard

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
	"github.com/autobutler-org/schmeide/internal/release"
)

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func jobState(subject string, state control.State, since time.Time) control.JobState {
	return control.JobState{Subject: subject, State: state, Repo: "o/r", Title: "Add a greeting", Since: since}
}

func TestBoardUpdateParsesJobsAndRunners(t *testing.T) {
	old := release.Version
	release.Version = "v1.2.0"
	t.Cleanup(func() { release.Version = old })

	b := newBoard("alice", "sonnet")
	b.update(control.Snapshot{
		Runners: []control.Registration{
			{Steward: "alice", Host: "a", Version: "v1.2.0"},
			{Steward: "alice", Host: "b", Version: "v1.1.0"},
			{Steward: "alice", Host: "c", Version: "dev"},
		},
		Jobs:     []control.JobState{jobState("schmeide.jobs.alice.42.7", control.StateWorking, t0)},
		Finished: []control.JobState{jobState("schmeide.jobs._pool.42.8", control.StateMerged, t0), jobState("odd", control.StateMerged, t0)},
	}, nil, t0.Add(time.Second))

	v := b.View()
	if !v.Connected || v.Error != "" || !v.Updated.Equal(t0.Add(time.Second)) || v.Steward != "alice" || v.Model != "sonnet" {
		t.Errorf("view header = connected %v, error %q, updated %s, steward %q, model %q", v.Connected, v.Error, v.Updated, v.Steward, v.Model)
	}
	var skewed []bool
	for _, r := range v.Runners {
		skewed = append(skewed, r.Skewed)
	}
	if want := []bool{false, true, false}; !reflect.DeepEqual(skewed, want) {
		t.Errorf("skewed = %v, want %v", skewed, want)
	}
	if got := v.Jobs[0]; got.RepoID != "42" || got.Issue != 7 || got.Pool {
		t.Errorf("job = %+v, want repo id 42, issue 7, not pool", got)
	}
	if got := v.Finished[0]; got.RepoID != "42" || got.Issue != 8 || !got.Pool {
		t.Errorf("finished job = %+v, want repo id 42, issue 8, pool", got)
	}
	if got := v.Finished[1]; got.RepoID != "" || got.Issue != 0 {
		t.Errorf("job with an odd subject = %+v, want no repo id or issue", got)
	}
}

func TestBoardActivity(t *testing.T) {
	const job = "schmeide.jobs.alice.42.7"
	b := newBoard("alice", "sonnet")
	snap := func(jobs, finished []control.JobState) control.Snapshot {
		return control.Snapshot{Jobs: jobs, Finished: finished}
	}
	b.update(snap([]control.JobState{jobState(job, control.StatePending, t0)}, nil), nil, t0)
	// The same state seen again adds nothing.
	b.update(snap([]control.JobState{jobState(job, control.StatePending, t0)}, nil), nil, t0)
	b.update(snap([]control.JobState{jobState(job, control.StateWorking, t0.Add(time.Minute))}, nil), nil, t0)
	b.update(snap(nil, []control.JobState{jobState(job, control.StateMerged, t0.Add(2*time.Minute))}), nil, t0)
	// A job queued again for the same issue enters pending at a new time.
	b.update(snap([]control.JobState{jobState(job, control.StatePending, t0.Add(3*time.Minute))}, nil), nil, t0)

	var got []string
	for _, e := range b.View().Activity {
		got = append(got, fmt.Sprintf("%s %s #%d %s", e.At.Format("15:04"), e.State, e.Issue, e.Title))
	}
	want := []string{
		"12:03 pending #7 Add a greeting",
		"12:02 merged #7 Add a greeting",
		"12:01 working #7 Add a greeting",
		"12:00 pending #7 Add a greeting",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("activity = %q, want %q", got, want)
	}
}

func TestBoardActivityKeepsTheNewest(t *testing.T) {
	b := newBoard("alice", "sonnet")
	var jobs []control.JobState
	for i := range maxActivity + 10 {
		jobs = append(jobs, jobState(fmt.Sprintf("schmeide.jobs.alice.42.%d", i), control.StateWorking, t0.Add(time.Duration(i)*time.Second)))
	}
	b.update(control.Snapshot{Jobs: jobs}, nil, t0)
	v := b.View()
	if len(v.Activity) != maxActivity || v.Activity[0].Issue != maxActivity+9 || v.Activity[maxActivity-1].Issue != 10 {
		t.Errorf("activity has %d events from #%d to #%d, want %d from #%d to #10",
			len(v.Activity), v.Activity[0].Issue, v.Activity[len(v.Activity)-1].Issue, maxActivity, maxActivity+9)
	}
	if len(b.seen) != maxActivity {
		t.Errorf("seen has %d keys, want %d", len(b.seen), maxActivity)
	}
}

func TestBoardFailedQueryKeepsTheLastGoodState(t *testing.T) {
	b := newBoard("alice", "sonnet")
	b.update(control.Snapshot{Jobs: []control.JobState{jobState("schmeide.jobs.alice.42.7", control.StateWorking, t0)}}, nil, t0)
	b.update(control.Snapshot{}, errors.New("nats: timeout"), t0.Add(time.Minute))

	v := b.View()
	if v.Connected || v.Error != "nats: timeout" {
		t.Errorf("connected %v, error %q; want disconnected with the error", v.Connected, v.Error)
	}
	if len(v.Jobs) != 1 || !v.Updated.Equal(t0) {
		t.Errorf("jobs %+v updated %s; want the last good state from %s", v.Jobs, v.Updated, t0)
	}
}
