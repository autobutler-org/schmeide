package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/nats-io/nats.go"
)

// A runner publishes its registration again on every interval while Run runs,
// so it outlives control.RegistrationTTL, and stops once ctx is canceled.
func TestRunnerKeepsItsRegistrationAlive(t *testing.T) {
	w := newWorld(t)
	registered := make(chan Registration, 64)
	if _, err := w.client.Subscribe(RegisterSubjectPrefix+"alice.*", func(m *nats.Msg) {
		var reg Registration
		if err := json.Unmarshal(m.Data, &reg); err != nil {
			t.Error(err)
		}
		registered <- reg
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.client.Flush(); err != nil {
		t.Fatal(err)
	}

	r, _ := w.runnerWith(t, "alice", &harness.Fake{}, false)
	receive := func(what string) Registration {
		t.Helper()
		select {
		case reg := <-registered:
			return reg
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s registration", what)
			return Registration{}
		}
	}
	first := receive("initial")

	const every = 20 * time.Millisecond
	r.registerEvery = every
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = r.Run(ctx)
	}()

	// More refreshes than fit in a TTL of three intervals: the registration
	// is still being renewed after it would have expired.
	for i := range 5 {
		if got := receive(fmt.Sprintf("refreshed #%d", i+1)); fmt.Sprint(got) != fmt.Sprint(first) {
			t.Errorf("refresh #%d = %+v, want the initial registration %+v", i+1, got, first)
		}
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx was canceled")
	}
	if err := w.client.Flush(); err != nil {
		t.Fatal(err)
	}
	// Drop anything published before Run returned, then expect silence.
	for len(registered) > 0 {
		<-registered
	}
	time.Sleep(5 * every)
	if n := len(registered); n != 0 {
		t.Errorf("runner published %d registrations after ctx was canceled, want 0", n)
	}
}
