package runner

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/schmeide/internal/harness"
	"github.com/autobutler-org/schmeide/internal/release"
)

// lockedBuffer is a bytes.Buffer the tracker's goroutine can write while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRegistrationVersionSkew(t *testing.T) {
	for _, c := range []struct {
		name, runner string
		warn         bool
	}{
		{"match", "v1.0.0", false},
		{"mismatch", "v1.1.0", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := release.Version
			release.Version = "v1.0.0"
			t.Cleanup(func() { release.Version = old })

			w := newWorld(t)
			var logs lockedBuffer
			tracker, err := w.queue.Track(slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			w.tracker = tracker
			r, err := New(t.Context(), Options{
				NATSURL:   w.natsURL,
				NATSToken: testRunnerTokens["alice"],
				Steward:   "alice",
				WorkDir:   t.TempDir(),
				Harnesses: []harness.Harness{&harness.Fake{}},
				AckWait:   time.Second,
				Version:   c.runner,
			}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("registration refused: %v", err)
			}
			t.Cleanup(r.Close)
			w.waitRegistered(t, "alice", false)

			got := strings.Contains(logs.String(), "runner version differs")
			if got != c.warn {
				t.Errorf("warning logged = %v, want %v; logs:\n%s", got, c.warn, logs.String())
			}
		})
	}
}
