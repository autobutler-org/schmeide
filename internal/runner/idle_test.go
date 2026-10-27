package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/harness"
)

// setIdleLimit shortens sessionIdleLimit for one test.
func setIdleLimit(t *testing.T, d time.Duration) {
	t.Helper()
	old := sessionIdleLimit
	sessionIdleLimit = d
	t.Cleanup(func() { sessionIdleLimit = old })
}

// chatty is a fake harness whose session writes a line every 20ms until its
// context ends or, when stopAfter is positive, until stopAfter has passed.
func chatty(stopAfter time.Duration) *harness.Fake {
	return &harness.Fake{RunFunc: func(ctx context.Context, req harness.Request) error {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var done <-chan time.Time
		if stopAfter > 0 {
			done = time.After(stopAfter)
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				return nil
			case <-tick.C:
				fmt.Fprintln(req.Output, "still working")
			}
		}
	}}
}

// TestSessionIdleLimitRetriesThenFails pins JN-FAIL-023: a session that writes
// nothing is killed at the idle limit, long before the session timeout, and
// the failure names the silence rather than a timeout. It retries like a
// session timeout.
func TestSessionIdleLimitRetriesThenFails(t *testing.T) {
	setIdleLimit(t, 200*time.Millisecond)
	w := newWorld(t)
	w.maxAttempts = 2
	h := &harness.Fake{RunFunc: func(ctx context.Context, _ harness.Request) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	r, _ := w.runner(t, "alice", h)
	r.opts.SessionTimeout = time.Hour
	w.run(t, r)
	w.publish(t, "alice", 7)

	var states, details []string
	for len(states) < 6 {
		select {
		case s := <-w.statuses:
			states = append(states, string(s.State))
			details = append(details, s.Detail)
		case <-time.After(30 * time.Second):
			t.Fatalf("saw states %v, want 6", states)
		}
	}
	if got, want := fmt.Sprint(states), "[claimed working retrying claimed working failed]"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
	if got, want := details[2], "attempt 1 of 2 failed: the fake session produced no output for 200ms"; got != want {
		t.Errorf("retrying detail = %q, want %q", got, want)
	}
	if got, want := details[5], "the fake session produced no output for 200ms"; got != want {
		t.Errorf("failed detail = %q, want %q", got, want)
	}
	_, comments := w.gh.Snapshot()
	want := "schmeide could not finish this job and did not open a pull request:\n\nthe fake session produced no output for 200ms\n"
	if len(comments["7"]) != 1 || !strings.HasPrefix(comments["7"][0], want) {
		t.Errorf("issue comments = %q, want one starting %q", comments["7"], want)
	}
	assertSettled(t, w)
}

func TestRunHarnessIdle(t *testing.T) {
	setIdleLimit(t, 150*time.Millisecond)
	r := &Runner{}
	tests := []struct {
		name    string
		h       *harness.Fake
		kind    string
		limit   time.Duration
		wantErr string
	}{
		{
			name: "silent reviewer is killed at the idle limit",
			h: &harness.Fake{RunFunc: func(ctx context.Context, _ harness.Request) error {
				<-ctx.Done()
				return ctx.Err()
			}},
			kind:    "fake reviewer",
			limit:   time.Hour,
			wantErr: "the fake reviewer session produced no output for 150ms",
		},
		{
			name:  "a session that keeps writing outlives the idle limit",
			h:     chatty(600 * time.Millisecond),
			kind:  "fake",
			limit: time.Hour,
		},
		{
			name:    "the session timeout still applies to a session that keeps writing",
			h:       chatty(0),
			kind:    "fake",
			limit:   400 * time.Millisecond,
			wantErr: "the fake session timed out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			err := r.runHarness(t.Context(), tt.h, tt.kind, tt.limit, harness.Request{})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("runHarness = %v, want nil", err)
				}
				return
			}
			var timedOut timeoutError
			if !errors.As(err, &timedOut) || err.Error() != tt.wantErr {
				t.Fatalf("runHarness = %v, want timeoutError %q", err, tt.wantErr)
			}
			if strings.Contains(tt.wantErr, "timed out") && time.Since(start) < tt.limit {
				t.Errorf("returned after %s, before the session timeout %s", time.Since(start), tt.limit)
			}
		})
	}
}
