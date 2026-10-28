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
	"github.com/nats-io/nats.go/jetstream"
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
	// Wait for the state first: the stream keeps one message per subject, so
	// the junk below could replace it before the tracker reads it.
	eventually(t, func() bool {
		status, _ := tracker.JobStatus(first.Subject())
		return status.State == StatePROpened
	})
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
	runner.publish(RegisterSubject("alice", alice.Host), alice)
	runner.publish(RegisterSubject("bob", bob.Host), bob)
	runner.publish(RegisterSubject("alice", aliceAgain.Host), aliceAgain)
	runner.publish(RegisterSubject("alice", alicePhone.Host), alicePhone)
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
	// are not recorded, and a restart must not bring them back. They go on
	// subjects with no valid status: the stream keeps one message per
	// subject, so junk on a job's subject would replace its stored status.
	runner.publish(RegisterSubjectPrefix+"mallory", alice)
	runner.publish(RegisterSubjectPrefix+"carol", []byte("not json"))
	runner.publish(StatusSubjectPrefix+"alice.7.3", []byte("not json"))
	runner.publish(StatusSubjectPrefix+"alice.7.3", Status{})
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
	for _, issue := range []int{2, 3} {
		junkOnly := Job{Repo: "o/r", RepoID: 7, Issue: issue, Steward: "alice"}
		if got, ok := tracker.JobStatus(junkOnly.Subject()); ok {
			t.Errorf("status of a junk-only job = %+v, want none", got)
		}
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

// startStatus starts a queue that `schmeide status` can reach, with its
// tracker and status service running.
func startStatus(t *testing.T) (*Queue, *Tracker, string) {
	t.Helper()
	q, url := startQueueOverTCP(t)
	tracker := startTracker(t, q)
	if err := q.ServeStatus(slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return q, tracker, url
}

func TestStatusListsRegisteredRunnerAndQueuedJob(t *testing.T) {
	q, tracker, url := startStatus(t)
	runner := startFakeRunner(t, q)

	hubot := Registration{Steward: "hubot", Harnesses: []string{"claude"}, Host: "hubot-laptop"}
	runner.publish(RegisterSubjectPrefix+"hubot", hubot)
	queued := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "hubot", InstallationID: 1, Title: "t"}
	done := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "hubot", InstallationID: 1, Title: "t"}
	runner.report(queued, StateClaimed, "")
	runner.report(done, StatePROpened, "https://github.com/o/r/pull/3")
	eventually(t, func() bool { _, ok := tracker.JobStatus(done.Subject()); return ok && len(tracker.Runners()) == 1 })

	text := runStatus(t, url)
	for _, want := range []string{"hubot", "hubot-laptop", "claude", queued.Subject(), string(StateClaimed)} {
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
	got.Jobs = withoutSince(t, got.Jobs)
	want := Snapshot{
		Runners: []Registration{hubot},
		Jobs:    []JobState{{Subject: queued.Subject(), State: StateClaimed}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--json = %+v, want %+v", got, want)
	}
}

func TestStatusShowsOnlyTheAskingStewardsRunnersAndJobs(t *testing.T) {
	q, tracker, url := startStatus(t)
	runner := startFakeRunner(t, q)

	hubot := Registration{Steward: "hubot", Harnesses: []string{"claude"}, Host: "hubot-laptop"}
	octocat := Registration{Steward: "octocat", Harnesses: []string{"grok"}, Host: "octo-secret-host", Version: "v9"}
	runner.publish(RegisterSubjectPrefix+"hubot", hubot)
	runner.publish(RegisterSubjectPrefix+"octocat", octocat)
	own := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "hubot"}
	theirs := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "octocat"}
	pooled := Job{Repo: "o/r", RepoID: 7, Issue: 3, Steward: "monalisa", Pool: true}
	runner.report(own, StateWorking, "claude")
	runner.report(theirs, StateRetrying, "attempt 1 of 3 failed: make check: secret gate output")
	runner.report(pooled, StateWorking, "grok")
	eventually(t, func() bool { _, ok := tracker.JobStatus(pooled.Subject()); return ok && len(tracker.Runners()) == 2 })

	var got Snapshot
	if err := json.Unmarshal([]byte(runStatus(t, url, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	want := Snapshot{
		Runners: []Registration{hubot},
		Jobs:    []JobState{{Subject: own.Subject(), State: StateWorking, Detail: "claude"}},
	}
	got.Jobs = withoutSince(t, got.Jobs)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--json = %+v, want only hubot's own runner and job: %+v", got, want)
	}
	if text := runStatus(t, url); strings.Contains(text, "octo") || strings.Contains(text, "secret") {
		t.Errorf("text output %q shows another steward's runner or job", text)
	}

	// A pool steward also sees pool jobs, which it may take.
	hubotPool := hubot
	hubotPool.Pool = true
	runner.publish(RegisterSubjectPrefix+"hubot", hubotPool)
	eventually(t, func() bool { return tracker.poolRunner("hubot") })
	if err := json.Unmarshal([]byte(runStatus(t, url, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	want = Snapshot{
		Runners: []Registration{hubotPool},
		Jobs: []JobState{
			{Subject: pooled.Subject(), State: StateWorking, Detail: "grok"},
			{Subject: own.Subject(), State: StateWorking, Detail: "claude"},
		},
	}
	got.Jobs = withoutSince(t, got.Jobs)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--json for a pool steward = %+v, want %+v", got, want)
	}
}

func TestStatusAsksTheControlNode(t *testing.T) {
	// No status service: an older control node, or none at all.
	_, url := startQueueOverTCP(t)
	var stdout, stderr bytes.Buffer
	code := StatusMain([]string{"--steward", "hubot", "--nats-token", "hubot-token", "--nats-url", url}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "upgrade") {
		t.Errorf("exit code = %d, stderr = %q; want 1 and advice to upgrade the control node", code, stderr.String())
	}
}

func TestStatusWithAnEmptyStreamSaysThereAreNoJobs(t *testing.T) {
	_, _, url := startStatus(t)
	if got := runStatus(t, url); !strings.Contains(got, "No queued or running jobs.") {
		t.Errorf("output = %q, want it to say there are no jobs", got)
	}
	if got := runStatus(t, url, "--json"); got != "{\n  \"runners\": [],\n  \"jobs\": []\n}\n" {
		t.Errorf("--json = %q, want empty runners and jobs", got)
	}
}

func TestStatusHidesEveryFinishedRunnerState(t *testing.T) {
	q, tracker, url := startStatus(t)
	runner := startFakeRunner(t, q)

	job := func(issue int) Job {
		return Job{Repo: "o/r", RepoID: 7, Issue: issue, Steward: "hubot", InstallationID: 1, Title: "t"}
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
	got.Jobs = withoutSince(t, got.Jobs)
	want := []JobState{
		{Subject: job(1).Subject(), State: StateWorking},
		{Subject: job(2).Subject(), State: StateReviewing},
	}
	if !reflect.DeepEqual(got.Jobs, want) {
		t.Errorf("jobs = %+v, want only the working and reviewing jobs", got.Jobs)
	}
}

// statusJobs runs `schmeide status --json` and returns its jobs by subject.
func statusJobs(t *testing.T, url string) map[string]JobState {
	t.Helper()
	var snap Snapshot
	if err := json.Unmarshal([]byte(runStatus(t, url, "--json")), &snap); err != nil {
		t.Fatal(err)
	}
	got := map[string]JobState{}
	for _, j := range snap.Jobs {
		got[j.Subject] = j
	}
	return got
}

func TestStatusListsAJobNoRunnerHasClaimed(t *testing.T) {
	q, tracker, url := startStatus(t)
	runner := startFakeRunner(t, q)
	runner.publish(RegisterSubjectPrefix+"hubot", Registration{Steward: "hubot", Harnesses: []string{"claude"}, Host: "laptop", Pool: true})
	eventually(t, func() bool { return tracker.hasRunner("hubot") })

	// hubot has a pool runner, so its job waits on the steward subject;
	// carol has none, so her job waits on the pool subject. runStatus reads
	// as hubot, who sees its own job and, as a pool steward, pool jobs.
	waiting := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "hubot", InstallationID: 1, Title: "t"}
	pooled := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "carol", InstallationID: 1, Title: "t", Pool: true}
	before := time.Now()
	for _, job := range []Job{waiting, pooled} {
		if created, err := q.Publish(t.Context(), job); err != nil || !created {
			t.Fatalf("Publish %s = %v, %v", job.Subject(), created, err)
		}
	}
	eventually(t, func() bool { _, ok := tracker.JobStatus(pooled.Subject()); return ok })

	got := statusJobs(t, url)
	for _, job := range []Job{waiting, pooled} {
		j, ok := got[job.Subject()]
		if !ok || j.State != StatePending {
			t.Fatalf("--json lists %s as %+v, %v; want pending", job.Subject(), j, ok)
		}
		if j.Since.Before(before.Add(-time.Second)) || j.Since.After(time.Now()) {
			t.Errorf("%s pending since %s, want the time it was published (after %s)", job.Subject(), j.Since, before)
		}
	}
	text := runStatus(t, url)
	for _, want := range []string{waiting.Subject(), pooled.Subject(), string(StatePending)} {
		if !strings.Contains(text, want) {
			t.Errorf("text output %q does not contain %q", text, want)
		}
	}

	// A duplicate delivery neither creates a job nor resets the wait.
	if created, err := q.Publish(t.Context(), waiting); err != nil || created {
		t.Fatalf("duplicate Publish = %v, %v", created, err)
	}
	if again := statusJobs(t, url)[waiting.Subject()]; !again.Since.Equal(got[waiting.Subject()].Since) {
		t.Errorf("duplicate delivery moved pending since from %s to %s", got[waiting.Subject()].Since, again.Since)
	}

	// A runner's report supersedes pending.
	runner.report(waiting, StateClaimed, "")
	eventually(t, func() bool { s, _ := tracker.JobStatus(waiting.Subject()); return s.State == StateClaimed })
	if j := statusJobs(t, url)[waiting.Subject()]; j.State != StateClaimed {
		t.Errorf("after the claim, %s is %+v, want claimed", waiting.Subject(), j)
	}
}

func TestPendingJobSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	q := startQueueIn(t, dir)
	startTracker(t, q)
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t", Pool: true}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}

	q = restartQueue(t, q, dir)
	tracker := startTracker(t, q)
	if got, ok := tracker.JobStatus(job.Subject()); !ok || got.State != StatePending {
		t.Errorf("status after restart = %+v, %v; want %s pending", got, ok, job.Subject())
	}
}

func TestPendingStatusFailureKeepsTheJob(t *testing.T) {
	q := startQueue(t)
	startTracker(t, q)
	// With the status stream no longer listening on job statuses, nothing
	// stores the pending status.
	if _, err := q.js.UpdateStream(t.Context(), jetstream.StreamConfig{
		Name: StatusStream, Subjects: []string{RegisterSubjectPrefix + "*"},
	}); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v; want the job created anyway", created, err)
	}
	if got := jobs(t, q); len(got) != 1 {
		t.Errorf("jobs = %+v, want one", got)
	}
}

// withoutSince checks that every job has a Since and clears it, so the rest
// can be compared exactly.
func withoutSince(t *testing.T, jobs []JobState) []JobState {
	t.Helper()
	for i := range jobs {
		if jobs[i].Since.IsZero() {
			t.Errorf("%s has no since", jobs[i].Subject)
		}
		jobs[i].Since = time.Time{}
	}
	return jobs
}

func TestFailedPublishLeavesNoPendingJob(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	// Without the job stream the job cannot be stored.
	if err := q.js.DeleteStream(t.Context(), JobStream); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	if _, err := q.Publish(t.Context(), job); err == nil {
		t.Fatal("Publish without a job stream succeeded")
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if len(tracker.jobs) != 0 {
		t.Errorf("job statuses = %+v, want none for a job that was never stored", tracker.jobs)
	}
}

// statusStreamInfo reads the status stream's current config and state.
func statusStreamInfo(t *testing.T, q *Queue) *jetstream.StreamInfo {
	t.Helper()
	stream, err := q.js.Stream(t.Context(), StatusStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestStatusStreamKeepsOnlyTheLatestMessagePerSubject(t *testing.T) {
	dir := t.TempDir()
	q := startQueueIn(t, dir)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	laptop := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice.laptop"}
	laptopAgain := Registration{Steward: "alice", Harnesses: []string{"claude", "fake"}, Host: "alice.laptop"}
	desk := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "desk", Pool: true}
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "alice"}
	runner.publish(RegisterSubject("alice", laptop.Host), laptop)
	runner.publish(RegisterSubject("alice", desk.Host), desk)
	runner.publish(RegisterSubject("alice", laptopAgain.Host), laptopAgain)
	// A registration whose subject names another host is not recorded.
	runner.publish(RegisterSubject("alice", "elsewhere"), laptop)
	runner.report(job, StateClaimed, "")
	runner.report(job, StateWorking, "")
	runner.report(job, StatePROpened, "https://github.com/o/r/pull/9")

	wantRunners := []Registration{desk, laptopAgain}
	wantStatus := Status{State: StatePROpened, Detail: "https://github.com/o/r/pull/9"}
	eventually(t, func() bool {
		status, _ := tracker.JobStatus(job.Subject())
		return status == wantStatus && reflect.DeepEqual(tracker.Runners(), wantRunners)
	})
	// One message each for the laptop, the desk, the other host, and the job.
	if got := statusStreamInfo(t, q).State.Msgs; got != 4 {
		t.Errorf("status stream holds %d messages, want 4", got)
	}

	// Replay skips the sequences the limit removed.
	q = restartQueue(t, q, dir)
	tracker = startTracker(t, q)
	if got := tracker.Runners(); !reflect.DeepEqual(got, wantRunners) {
		t.Errorf("runners after restart = %+v, want %+v", got, wantRunners)
	}
	if !tracker.poolRunner("alice") {
		t.Error("the desk's pool registration was lost on restart")
	}
	if got, ok := tracker.JobStatus(job.Subject()); !ok || got != wantStatus {
		t.Errorf("status after restart = %+v, %v; want %+v", got, ok, wantStatus)
	}
	if snap := tracker.Snapshot("alice", StatusQuery{}); !reflect.DeepEqual(snap.Runners, wantRunners) {
		t.Errorf("Snapshot runners = %+v, want %+v", snap.Runners, wantRunners)
	}
}

func TestTrackLimitsAStatusStreamCreatedWithoutLimits(t *testing.T) {
	q := startQueue(t)
	// The status stream as an older control node created it.
	if _, err := q.js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name:     StatusStream,
		Subjects: []string{RegisterSubjectPrefix + "*", StatusSubjectPrefix + "*.*.*"},
	}); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "alice"}
	subject := StatusSubjectPrefix + strings.TrimPrefix(job.Subject(), jobSubjectPrefix)
	for _, state := range []State{StateClaimed, StateWorking, StatePROpened} {
		data, _ := json.Marshal(Status{State: state})
		if _, err := q.js.Publish(t.Context(), subject, data); err != nil {
			t.Fatal(err)
		}
	}

	tracker := startTracker(t, q)
	info := statusStreamInfo(t, q)
	if info.Config.MaxMsgsPerSubject != 1 || info.Config.Discard != jetstream.DiscardOld {
		t.Errorf("limits = %d per subject, discard %v; want 1 and old", info.Config.MaxMsgsPerSubject, info.Config.Discard)
	}
	if info.State.Msgs != 1 {
		t.Errorf("status stream holds %d messages after the update, want 1", info.State.Msgs)
	}
	if got, ok := tracker.JobStatus(job.Subject()); !ok || got.State != StatePROpened {
		t.Errorf("status = %+v, %v; want the latest, pr_opened", got, ok)
	}
}

// advance moves the tracker's clock d past the real time.
func advance(tracker *Tracker, d time.Duration) {
	tracker.setClock(func() time.Time { return time.Now().Add(d) })
}

func TestRegistrationExpiresWithoutRefresh(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	alice := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice-laptop", Pool: true}
	runner.publish(RegisterSubjectPrefix+"alice", alice)
	eventually(t, func() bool { return tracker.hasRunner("alice") })

	advance(tracker, RegistrationTTL-time.Second)
	if !tracker.hasRunner("alice") || !tracker.poolRunner("alice") || len(tracker.Runners()) != 1 {
		t.Fatalf("registration expired before its TTL: runners = %+v", tracker.Runners())
	}
	advance(tracker, RegistrationTTL+time.Second)
	if tracker.hasRunner("alice") || tracker.poolRunner("alice") {
		t.Error("an expired registration still counts as a runner")
	}
	if got := tracker.Runners(); len(got) != 0 {
		t.Errorf("runners = %+v, want none once expired", got)
	}

	// A refresh brings it back.
	tracker.setClock(time.Now)
	runner.publish(RegisterSubjectPrefix+"alice", alice)
	eventually(t, func() bool { return tracker.hasRunner("alice") })
	if got := tracker.Runners(); !reflect.DeepEqual(got, []Registration{alice}) {
		t.Errorf("runners after a refresh = %+v, want %+v", got, []Registration{alice})
	}
}

func TestExpiredRegistrationSendsTheNextJobToThePool(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)

	runner.publish(RegisterSubjectPrefix+"octocat", Registration{Steward: "octocat", Harnesses: []string{"fake"}, Host: "h"})
	eventually(t, func() bool { return tracker.hasRunner("octocat") })
	first := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	second := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "octocat", InstallationID: 1, Title: "t"}
	if created, err := q.Publish(t.Context(), first); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	advance(tracker, RegistrationTTL+time.Second)
	if created, err := q.Publish(t.Context(), second); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	got := jobs(t, q)
	if len(got) != 2 || got[0].Pool || !got[1].Pool {
		t.Fatalf("jobs = %+v, want the first on the steward subject and the second pooled", got)
	}
	if got[1].Subject() != jobSubjectPrefix+PoolName+".7.2" {
		t.Errorf("second job subject = %s, want the pool subject", got[1].Subject())
	}
}

func TestRestartKeepsOnlyLiveRegistrations(t *testing.T) {
	dir := t.TempDir()
	q := startQueueIn(t, dir)
	tracker := startTracker(t, q)
	alice := Registration{Steward: "alice", Harnesses: []string{"claude"}, Host: "alice-laptop"}
	startFakeRunner(t, q).publish(RegisterSubjectPrefix+"alice", alice)
	eventually(t, func() bool { return tracker.hasRunner("alice") })

	tracker = startTracker(t, restartQueue(t, q, dir))
	if got := tracker.Runners(); !reflect.DeepEqual(got, []Registration{alice}) {
		t.Errorf("runners right after a restart = %+v, want %+v", got, []Registration{alice})
	}
	// The registration's age comes from when it was stored, not from the
	// replay, so a restart does not refresh it.
	advance(tracker, RegistrationTTL+time.Second)
	if got := tracker.Runners(); len(got) != 0 {
		t.Errorf("runners = %+v, want none once the stored registration is older than the TTL", got)
	}
}

func TestStatusCarriesRepoAndTitle(t *testing.T) {
	q, tracker, url := startStatus(t)
	runner := startFakeRunner(t, q)
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "hubot", InstallationID: 1, Title: "Add a greeting", Pool: true}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	eventually(t, func() bool { _, ok := tracker.JobStatus(job.Subject()); return ok })
	runner.publish(RegisterSubjectPrefix+"hubot", Registration{Steward: "hubot", Harnesses: []string{"claude"}, Host: "laptop", Pool: true})
	eventually(t, func() bool { return tracker.poolRunner("hubot") })

	if got := statusJobs(t, url)[job.Subject()]; got.Repo != job.Repo || got.Title != job.Title {
		t.Errorf("pending job = %+v, want repo %q and title %q", got, job.Repo, job.Title)
	}

	// An older runner's status names neither, so the pending status's are kept.
	runner.report(job, StateWorking, "")
	eventually(t, func() bool { s, _ := tracker.JobStatus(job.Subject()); return s.State == StateWorking })
	if got := statusJobs(t, url)[job.Subject()]; got.Repo != job.Repo || got.Title != job.Title {
		t.Errorf("working job from an older runner = %+v, want repo %q and title %q kept", got, job.Repo, job.Title)
	}

	// A runner's own repo and title win.
	runner.publish(StatusSubjectPrefix+strings.TrimPrefix(job.Subject(), jobSubjectPrefix), Status{State: StateReviewing, Repo: "o/renamed", Title: "Add a better greeting"})
	eventually(t, func() bool { s, _ := tracker.JobStatus(job.Subject()); return s.State == StateReviewing })
	if got := statusJobs(t, url)[job.Subject()]; got.Repo != "o/renamed" || got.Title != "Add a better greeting" {
		t.Errorf("reviewing job = %+v, want the runner's repo and title", got)
	}
}

// queryFinished asks for the steward's snapshot with finished jobs.
func queryFinished(t *testing.T, url, steward string) Snapshot {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo(steward, steward+"-token"), nats.CustomInboxPrefix(InboxPrefix(steward)))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	snap, err := QueryStatus(nc, steward, StatusQuery{Finished: true})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestStatusQueryListsFinishedJobsOnRequest(t *testing.T) {
	q, tracker, url := startStatus(t)
	runner := startFakeRunner(t, q)
	job := func(steward string, issue int) Job {
		return Job{Repo: "o/r", RepoID: 7, Issue: issue, Steward: steward, InstallationID: 1, Title: "t"}
	}
	runner.report(job("hubot", 1), StateWorking, "")
	runner.report(job("hubot", 2), StateMerged, "https://github.com/o/r/pull/3")
	runner.report(job("hubot", 3), StateGateFailed, "make check failed")
	runner.report(job("carol", 4), StateMerged, "https://github.com/o/r/pull/5")
	eventually(t, func() bool { _, ok := tracker.JobStatus(job("carol", 4).Subject()); return ok })

	snap := queryFinished(t, url, "hubot")
	snap.Jobs = withoutSince(t, snap.Jobs)
	snap.Finished = withoutSince(t, snap.Finished)
	wantJobs := []JobState{{Subject: job("hubot", 1).Subject(), State: StateWorking}}
	wantFinished := []JobState{
		{Subject: job("hubot", 2).Subject(), State: StateMerged, Detail: "https://github.com/o/r/pull/3"},
		{Subject: job("hubot", 3).Subject(), State: StateGateFailed, Detail: "make check failed"},
	}
	if !reflect.DeepEqual(snap.Jobs, wantJobs) || !reflect.DeepEqual(snap.Finished, wantFinished) {
		t.Errorf("snapshot = %+v, want jobs %+v and finished %+v", snap, wantJobs, wantFinished)
	}

	// `schmeide status` does not ask, so its reply has no finished jobs.
	var plain map[string]json.RawMessage
	if err := json.Unmarshal([]byte(runStatus(t, url, "--json")), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["finished"]; ok {
		t.Errorf("schmeide status --json = %v, want no finished key", plain)
	}

	// A job that finished longer ago than FinishedWindow is left out.
	advance(tracker, FinishedWindow+time.Second)
	if got := queryFinished(t, url, "hubot").Finished; len(got) != 0 {
		t.Errorf("finished after the window = %+v, want none", got)
	}
}
