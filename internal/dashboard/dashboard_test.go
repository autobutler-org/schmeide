package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
)

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// startControl starts a control node queue that answers status queries.
func startControl(t *testing.T) (*control.Queue, string) {
	t.Helper()
	addr := freeAddr(t)
	q, err := control.StartQueue(t.Context(), control.QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: map[string]string{"alice": "alice-token"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	log := slog.New(slog.DiscardHandler)
	if _, err := q.Track(log); err != nil {
		t.Fatal(err)
	}
	if err := q.ServeStatus(log); err != nil {
		t.Fatal(err)
	}
	return q, "nats://" + addr
}

// getJSON fetches url into v, retrying until the server answers.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			defer res.Body.Close()
			if err := json.NewDecoder(res.Body).Decode(v); err != nil {
				t.Fatal(err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunServesTheStewardsJobsAndAnswers(t *testing.T) {
	q, natsURL := startControl(t)
	job := control.Job{Repo: "o/r", RepoID: 42, Issue: 7, Steward: "alice", InstallationID: 1, Title: "Add a greeting", Pool: true}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	t.Setenv("FAKEAGENT_SCRIPT", "stdout #7 has waited for a runner.")

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())
	var logs bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Steward: "alice", NATSURL: natsURL, NATSToken: "alice-token",
			Addr: addr, Interval: 20 * time.Millisecond, Model: "sonnet", Claude: fakeagentBin,
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	// alice has no pool runner, so her pool job is not hers to see; it shows
	// once she registers one, as in `schmeide status`.
	reg, _ := json.Marshal(control.Registration{Steward: "alice", Harnesses: []string{"claude-code"}, Host: "laptop", Pool: true})
	if err := q.Conn().Publish(control.RegisterSubject("alice", "laptop"), reg); err != nil {
		t.Fatal(err)
	}
	var v View
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		getJSON(t, "http://"+addr+"/api/state", &v)
		if len(v.Jobs) == 1 && len(v.Runners) == 1 {
			break
		}
	}
	if !v.Connected || len(v.Runners) != 1 || len(v.Jobs) != 1 {
		t.Fatalf("state = %+v, want alice's runner and pending pool job", v)
	}
	if j := v.Jobs[0]; j.State != control.StatePending || j.Repo != "o/r" || j.Title != "Add a greeting" || j.Issue != 7 || !j.Pool {
		t.Errorf("job = %+v, want pending o/r #7 with its title", j)
	}
	if len(v.Activity) != 1 || v.Activity[0].State != control.StatePending {
		t.Errorf("activity = %+v, want the pending state", v.Activity)
	}

	body := strings.NewReader(`{"question":"Is #7 stuck?"}`)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/api/ask", body)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var answer map[string]string
	_ = json.NewDecoder(res.Body).Decode(&answer)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || answer["answer"] != "#7 has waited for a runner." {
		t.Errorf("ask = %d %v", res.StatusCode, answer)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
	if !strings.Contains(logs.String(), "dashboard serving") {
		t.Errorf("logs = %q, want the serving line", logs.String())
	}
}

func TestRunFailsWithoutAControlNode(t *testing.T) {
	err := Run(t.Context(), Config{Steward: "alice", NATSURL: "nats://" + freeAddr(t), Addr: freeAddr(t), Interval: time.Second, Model: "sonnet"}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "Check that `schmeide control` is running") {
		t.Errorf("Run = %v, want it to say what to check", err)
	}
}

func TestRunFailsWhenTheAddressIsTaken(t *testing.T) {
	_, natsURL := startControl(t)
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	err = Run(t.Context(), Config{Steward: "alice", NATSURL: natsURL, NATSToken: "alice-token", Addr: l.Addr().String(), Interval: time.Second, Model: "sonnet"}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "pass a free address with --addr") {
		t.Errorf("Run = %v, want it to ask for a free --addr", err)
	}
}
