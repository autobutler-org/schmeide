package control

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkWebhookIntake measures one signed, ready-labeled delivery through
// Handler: the signature check, the decode, and a Publish that stores nothing.
func BenchmarkWebhookIntake(b *testing.B) {
	body, err := os.ReadFile(filepath.Join("testdata", "issues_assigned_labeled.json"))
	if err != nil {
		b.Fatal(err)
	}
	var published int
	h := Handler(Intake{
		Secret:     []byte(testSecret),
		ReadyLabel: DefaultReadyLabel,
		Allow:      Allowlist{Orgs: []string{"autobutler-org"}},
		Publish: func(context.Context, Job) (bool, error) {
			published++
			return true, nil
		},
	}, slog.New(slog.DiscardHandler), WebhookPolicy{})
	signature := sign(body)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-Github-Event", "issues")
		req.Header.Set("X-Hub-Signature-256", signature)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
			b.Fatalf("status = %d, want a 2xx", rec.Code)
		}
	}
	if published != b.N {
		b.Fatalf("published %d jobs, want %d", published, b.N)
	}
}

// BenchmarkStatusStore records 100 job statuses on the in-process queue, lets
// a Tracker apply them, and measures listing them with Tracker.Snapshot.
func BenchmarkStatusStore(b *testing.B) {
	const jobCount = 100
	q, err := StartQueue(b.Context(), QueueOptions{DataDir: b.TempDir(), RunnerTokens: testRunnerTokens})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(q.Close)
	// The first Track creates the status stream. Statuses stored before the
	// second Track are applied by its replay before it returns, so the
	// benchmark never waits on a live consumer.
	if _, err := q.Track(slog.New(slog.DiscardHandler)); err != nil {
		b.Fatal(err)
	}
	for i := range jobCount {
		job := Job{Repo: "autobutler-org/quark", Issue: i + 1, Steward: "octocat"}
		subject := StatusSubjectPrefix + job.Subject()[len(jobSubjectPrefix):]
		if _, err := q.js.Publish(b.Context(), subject, []byte(`{"state":"working"}`)); err != nil {
			b.Fatal(err)
		}
	}
	tracker, err := q.Track(slog.New(slog.DiscardHandler))
	if err != nil {
		b.Fatal(err)
	}
	if _, ok := tracker.JobStatus(Job{Repo: "autobutler-org/quark", Issue: jobCount, Steward: "octocat"}.Subject()); !ok {
		b.Fatal("tracker did not apply the recorded statuses")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		snap := tracker.Snapshot("octocat")
		if len(snap.Jobs) != jobCount {
			b.Fatalf("listed %d jobs, want %d", len(snap.Jobs), jobCount)
		}
	}
}
