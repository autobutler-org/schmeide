package control

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// fakeRunner publishes what a runner would. It uses the control node's own
// connection, so it can publish for any steward and the payload checks are
// what is under test.
type fakeRunner struct {
	t  *testing.T
	nc *nats.Conn
}

func startFakeRunner(t *testing.T, q *Queue) fakeRunner {
	t.Helper()
	return fakeRunner{t: t, nc: q.Conn()}
}

func (f fakeRunner) publish(subject string, v any) {
	f.t.Helper()
	data, ok := v.([]byte)
	if !ok {
		var err error
		if data, err = json.Marshal(v); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := f.nc.Publish(subject, data); err != nil {
		f.t.Fatal(err)
	}
}

func (f fakeRunner) report(job Job, state State, detail string) {
	f.t.Helper()
	f.publish(StatusSubjectPrefix+strings.TrimPrefix(job.Subject(), jobSubjectPrefix), Status{State: state, Detail: detail})
}

// eventually waits for the control node to catch up with the fake runner.
func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
}

func startTracker(t *testing.T, q *Queue) *Tracker {
	t.Helper()
	tracker, err := q.Track(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return tracker
}

func TestTrackerRecordsRegisteredRunners(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	alice := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice-laptop"}
	bob := Registration{Steward: "bob", Harnesses: []string{"claude"}, Host: "bob-desktop"}
	aliceAgain := Registration{Steward: "alice", Harnesses: []string{"claude", "fake"}, Host: "alice-laptop"}
	runner.publish(RegisterSubjectPrefix+"alice", alice)
	runner.publish(RegisterSubjectPrefix+"bob", bob)
	// Neither a registration for someone else's subject nor junk is recorded.
	runner.publish(RegisterSubjectPrefix+"mallory", alice)
	runner.publish(RegisterSubjectPrefix+"carol", []byte("not json"))
	runner.publish(RegisterSubjectPrefix+"alice", aliceAgain)

	want := []Registration{bob, aliceAgain}
	eventually(t, func() bool { return reflect.DeepEqual(tracker.Runners(), want) })
	if got := tracker.Runners(); !reflect.DeepEqual(got, want) {
		t.Errorf("runners = %+v, want %+v", got, want)
	}
}

func TestTrackerReportsLatestStatePerJob(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	first := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "alice"}
	second := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "bob"}
	untouched := Job{Repo: "o/r", RepoID: 7, Issue: 3, Steward: "alice"}
	runner.report(first, StateClaimed, "")
	runner.report(second, StateClaimed, "")
	runner.report(first, StateWorking, "")
	runner.report(first, StatePROpened, "https://github.com/o/r/pull/9")
	// Junk does not overwrite a job's state.
	runner.publish(StatusSubjectPrefix+"alice.7.1", []byte("not json"))
	runner.publish(StatusSubjectPrefix+"alice.7.1", Status{})
	runner.report(second, StateGateFailed, "make check failed")
	eventually(t, func() bool {
		status, _ := tracker.JobStatus(second.Subject())
		return status.State == StateGateFailed
	})

	for job, want := range map[Job]Status{
		first:  {State: StatePROpened, Detail: "https://github.com/o/r/pull/9"},
		second: {State: StateGateFailed, Detail: "make check failed"},
	} {
		if got, ok := tracker.JobStatus(job.Subject()); !ok || got != want {
			t.Errorf("status of %s = %+v, %v; want %+v", job.Subject(), got, ok, want)
		}
	}
	if got, ok := tracker.JobStatus(untouched.Subject()); ok {
		t.Errorf("status of a job nobody reported = %+v, want none", got)
	}
}
