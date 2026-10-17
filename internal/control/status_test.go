package control

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
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
	bob := Registration{Steward: "bob", Harnesses: []string{"claude"}, Host: "bob-desktop", Pool: true}
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

func TestTrackerRemembersRunnersAndStatusAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	q := startQueueIn(t, dir)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	alice := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice-laptop"}
	bob := Registration{Steward: "bob", Harnesses: []string{"claude"}, Host: "bob-desktop", Pool: true}
	aliceAgain := Registration{Steward: "alice", Harnesses: []string{"claude", "fake"}, Host: "alice-laptop"}
	alicePhone := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice-phone"}
	first := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "alice"}
	untouched := Job{Repo: "o/r", RepoID: 7, Issue: 3, Steward: "alice"}
	runner.publish(RegisterSubjectPrefix+"alice", alice)
	runner.publish(RegisterSubjectPrefix+"bob", bob)
	runner.publish(RegisterSubjectPrefix+"alice", aliceAgain)
	runner.publish(RegisterSubjectPrefix+"alice", alicePhone)
	runner.report(first, StateClaimed, "")
	runner.report(first, StatePROpened, "https://github.com/o/r/pull/9")

	wantRunners := []Registration{bob, aliceAgain, alicePhone}
	wantStatus := Status{State: StatePROpened, Detail: "https://github.com/o/r/pull/9"}
	eventually(t, func() bool {
		status, ok := tracker.JobStatus(first.Subject())
		return ok && status == wantStatus && reflect.DeepEqual(tracker.Runners(), wantRunners)
	})
	if got := tracker.Runners(); !reflect.DeepEqual(got, wantRunners) {
		t.Fatalf("runners before restart = %+v, want %+v", got, wantRunners)
	}

	q = restartQueue(t, q, dir)
	tracker = startTracker(t, q)
	if got := tracker.Runners(); !reflect.DeepEqual(got, wantRunners) {
		t.Errorf("runners = %+v, want %+v", got, wantRunners)
	}
	if got, ok := tracker.JobStatus(first.Subject()); !ok || got != wantStatus {
		t.Errorf("status of %s = %+v, %v; want %+v", first.Subject(), got, ok, wantStatus)
	}
	if got, ok := tracker.JobStatus(untouched.Subject()); ok {
		t.Errorf("status of a job nobody reported = %+v, want none", got)
	}

	// A report after the restart is still recorded.
	startFakeRunner(t, q).report(first, StateFailed, "gave up")
	eventually(t, func() bool {
		status, _ := tracker.JobStatus(first.Subject())
		return status.State == StateFailed
	})
	if got, ok := tracker.JobStatus(first.Subject()); !ok || got != (Status{State: StateFailed, Detail: "gave up"}) {
		t.Errorf("status after restart = %+v, %v; want failed", got, ok)
	}
}

func TestTrackerRestartDropsMalformed(t *testing.T) {
	dir := t.TempDir()
	q := startQueueIn(t, dir)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	alice := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice-laptop"}
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "alice"}
	runner.publish(RegisterSubjectPrefix+"alice", alice)
	runner.report(job, StateWorking, "")
	// A registration for someone else's subject, junk, and an empty status
	// are not recorded, and a restart must not bring them back.
	runner.publish(RegisterSubjectPrefix+"mallory", alice)
	runner.publish(RegisterSubjectPrefix+"carol", []byte("not json"))
	runner.publish(StatusSubjectPrefix+"alice.7.1", []byte("not json"))
	runner.publish(StatusSubjectPrefix+"alice.7.1", Status{})
	runner.publish(StatusSubjectPrefix+"alice.7.2", []byte("not json"))

	want := []Registration{alice}
	wantStatus := Status{State: StateWorking}
	eventually(t, func() bool {
		status, ok := tracker.JobStatus(job.Subject())
		return ok && status == wantStatus && reflect.DeepEqual(tracker.Runners(), want)
	})
	if got := tracker.Runners(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runners before restart = %+v, want %+v", got, want)
	}

	tracker = startTracker(t, restartQueue(t, q, dir))
	if got := tracker.Runners(); !reflect.DeepEqual(got, want) {
		t.Errorf("runners = %+v, want %+v", got, want)
	}
	if got, ok := tracker.JobStatus(job.Subject()); !ok || got != wantStatus {
		t.Errorf("status of %s = %+v, %v; want %+v", job.Subject(), got, ok, wantStatus)
	}
	junkOnly := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "alice"}
	if got, ok := tracker.JobStatus(junkOnly.Subject()); ok {
		t.Errorf("status of a junk-only job = %+v, want none", got)
	}
}

func startQueueIn(t *testing.T, dir string) *Queue {
	t.Helper()
	q, err := StartQueue(t.Context(), QueueOptions{DataDir: dir, RunnerTokens: testRunnerTokens})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q
}

func restartQueue(t *testing.T, q *Queue, dir string) *Queue {
	t.Helper()
	q.Close()
	return startQueueIn(t, dir)
}

// startQueueOverTCP starts a queue that runners, and `schmeide status`, can
// reach at the returned NATS URL.
func startQueueOverTCP(t *testing.T) (*Queue, string) {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	q, err := StartQueue(t.Context(), QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: testRunnerTokens})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q, "nats://" + addr
}

func runStatus(t *testing.T, url string, extra ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"--steward", "hubot", "--nats-token", "hubot-token", "--nats-url", url}, extra...)
	if code := StatusMain(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	return stdout.String()
}

func TestStatusListsRegisteredRunnerAndQueuedJob(t *testing.T) {
	q, url := startQueueOverTCP(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	octocat := Registration{Steward: "octocat", Harnesses: []string{"claude"}, Host: "octo-laptop"}
	runner.publish(RegisterSubjectPrefix+"octocat", octocat)
	queued := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	done := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "octocat", InstallationID: 1, Title: "t"}
	runner.report(queued, StateClaimed, "")
	runner.report(done, StatePROpened, "https://github.com/o/r/pull/3")
	eventually(t, func() bool { _, ok := tracker.JobStatus(done.Subject()); return ok })

	text := runStatus(t, url)
	for _, want := range []string{"octocat", "octo-laptop", "claude", queued.Subject(), string(StateClaimed)} {
		if !strings.Contains(text, want) {
			t.Errorf("text output %q does not contain %q", text, want)
		}
	}
	if strings.Contains(text, done.Subject()) {
		t.Errorf("text output %q lists the finished job", text)
	}

	var got Snapshot
	if err := json.Unmarshal([]byte(runStatus(t, url, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	want := Snapshot{
		Runners: []Registration{octocat},
		Jobs:    []JobState{{Subject: queued.Subject(), State: StateClaimed}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--json = %+v, want %+v", got, want)
	}
}

func TestStatusWithAnEmptyStreamSaysThereAreNoJobs(t *testing.T) {
	q, url := startQueueOverTCP(t)
	startTracker(t, q)
	if got := runStatus(t, url); !strings.Contains(got, "No queued or running jobs.") {
		t.Errorf("output = %q, want it to say there are no jobs", got)
	}
	if got := runStatus(t, url, "--json"); got != "{\n  \"runners\": [],\n  \"jobs\": []\n}\n" {
		t.Errorf("--json = %q, want empty runners and jobs", got)
	}
}

func TestStatusHidesEveryFinishedRunnerState(t *testing.T) {
	q, url := startQueueOverTCP(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	job := func(issue int) Job {
		return Job{Repo: "o/r", RepoID: 7, Issue: issue, Steward: "octocat", InstallationID: 1, Title: "t"}
	}
	states := []State{StateWorking, StateReviewing, StateMerged, StateMergeFailed, StateReviewFailed, StateQueued}
	for i, state := range states {
		runner.report(job(i+1), state, "")
	}
	last := job(len(states)).Subject()
	eventually(t, func() bool { _, ok := tracker.JobStatus(last); return ok })

	var got Snapshot
	if err := json.Unmarshal([]byte(runStatus(t, url, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	want := []JobState{
		{Subject: job(1).Subject(), State: StateWorking},
		{Subject: job(2).Subject(), State: StateReviewing},
	}
	if !reflect.DeepEqual(got.Jobs, want) {
		t.Errorf("jobs = %+v, want only the working and reviewing jobs", got.Jobs)
	}
}
