package harness

import "context"

// Fake is a [Harness] for tests. It starts no process.
type Fake struct {
	// HarnessName is returned from Name. Empty means "fake".
	HarnessName string
	// CheckErr is returned from Check.
	CheckErr error
	// RunFunc is called by Run. Nil makes Run succeed, or fail with the
	// context's error once the context is canceled.
	RunFunc func(ctx context.Context, req Request) error
	// Requests records every request passed to Run. Run does not lock, so
	// read it only after Run returns.
	Requests []Request
}

// Name implements [Harness].
func (f *Fake) Name() string {
	if f.HarnessName == "" {
		return "fake"
	}
	return f.HarnessName
}

// Check implements [Harness].
func (f *Fake) Check(context.Context) error { return f.CheckErr }

// Run implements [Harness].
func (f *Fake) Run(ctx context.Context, req Request) error {
	f.Requests = append(f.Requests, req)
	if f.RunFunc != nil {
		return f.RunFunc(ctx, req)
	}
	return ctx.Err()
}
