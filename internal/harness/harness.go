// Package harness defines how a runner drives an agent harness, which is an
// agent CLI such as Claude Code, and holds the implementations.
//
// # Contract
//
// Every harness must:
//
//   - Use the login already present on the machine. schmeide never handles
//     agent API keys.
//   - Report from Check, without starting a session, whether it can run. A
//     missing CLI wraps [ErrNotInstalled] and a missing login wraps
//     [ErrNotLoggedIn], and both messages say how to fix the problem.
//   - Start a fresh session on every Run. No conversation state carries over
//     from an earlier Run.
//   - Run without a terminal and never wait for input from a person.
//   - Stay inside Request.Dir. The harness must not create, change, or delete
//     files outside that directory.
//   - Return nil from Run only when the session finished successfully, and a
//     non-nil error for every other outcome.
//   - Stop when the context given to Run is canceled, and return an error
//     that wraps the context's error.
//   - Leave no process behind. When Run returns, for any reason, every
//     process the session started has been killed.
//
// Optional:
//
//   - Streaming. A harness may write output to Request.Output as it is
//     produced or all at once at the end. The format of the output is up to
//     the harness, and callers must not parse it.
//   - Model choice, permission modes, and other settings, which belong to the
//     implementation and not to this interface.
//
// The package runs on Unix only, because killing a session relies on process
// groups.
package harness

import (
	"context"
	"errors"
	"io"
)

var (
	// ErrNotInstalled means the harness CLI was not found on this machine.
	ErrNotInstalled = errors.New("harness not installed")
	// ErrNotLoggedIn means the harness CLI is installed but has no login.
	ErrNotLoggedIn = errors.New("harness not logged in")
)

// Request describes one session.
type Request struct {
	// Dir is the working directory the session runs in and must stay inside.
	Dir string
	// Prompt is the full brief for the session.
	Prompt string
	// Output receives the session's output. Nil discards it.
	Output io.Writer
}

// Harness is the adapter a runner uses to drive one agent CLI. See the
// package documentation for what an implementation must guarantee.
type Harness interface {
	// Name identifies the harness when a runner advertises it, for example
	// "claude-code".
	Name() string
	// Check returns nil when the harness is installed and logged in.
	Check(ctx context.Context) error
	// Run starts a fresh non-interactive session and blocks until it ends.
	// Cancel ctx to cancel the session.
	Run(ctx context.Context, req Request) error
}

// All returns every harness schmeide knows about, usable or not.
func All() []Harness {
	return []Harness{&ClaudeCode{}}
}

// Available returns the harnesses from hs that pass Check, which is the list
// a runner advertises when it registers.
func Available(ctx context.Context, hs []Harness) []Harness {
	var ok []Harness
	for _, h := range hs {
		if h.Check(ctx) == nil {
			ok = append(ok, h)
		}
	}
	return ok
}
