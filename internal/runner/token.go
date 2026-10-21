package runner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/autobutler-org/schmeide/internal/control"
)

// tokenRefreshMargin is how long before its expiry a job's GitHub token is
// replaced with a new one from the control node.
const tokenRefreshMargin = 5 * time.Minute

// jobToken is one job's GitHub installation token. GitHub expires one an
// hour after it is minted, and a job can run longer, so get asks the control
// node for a new token whenever the one held is within tokenRefreshMargin of
// its expiry. Every GitHub call and git command of the job goes through get.
type jobToken struct {
	// fetch asks the control node for a token for the job.
	fetch func(context.Context) (control.Token, error)

	mu  sync.Mutex
	cur control.Token
}

// get returns a token that is not about to expire. A failed refresh returns
// an error and keeps the old token, so the next call tries again.
func (j *jobToken) get(ctx context.Context) (string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !stale(j.cur, time.Now()) {
		return j.cur.Token, nil
	}
	token, err := j.fetch(ctx)
	if err != nil {
		return "", fmt.Errorf("refresh the job's GitHub token: %w", err)
	}
	j.cur = token
	return token.Token, nil
}

// stale reports whether token must be replaced at now: there is none yet,
// or it expires within tokenRefreshMargin. A token without an expiry is
// never stale.
func stale(token control.Token, now time.Time) bool {
	if token.Token == "" {
		return true
	}
	return !token.ExpiresAt.IsZero() && !now.Before(token.ExpiresAt.Add(-tokenRefreshMargin))
}
