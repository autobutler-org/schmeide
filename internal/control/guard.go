package control

import (
	"errors"
	"sync"
	"time"
)

// errRateLimited is logged when a webhook arrives after the per-minute cap.
var errRateLimited = errors.New("too many webhook requests")

// webhookGate is the handler's in-memory rate limit and delivery-id memory.
type webhookGate struct {
	limit int
	ttl   time.Duration
	now   func() time.Time

	mu     sync.Mutex
	window time.Time
	used   int
	seen   map[string]time.Time
}

func newWebhookGate(p WebhookPolicy) *webhookGate {
	now := p.now
	if now == nil {
		now = time.Now
	}
	return &webhookGate{
		limit: p.RequestsPerMinute,
		ttl:   p.DeliveryTTL,
		now:   now,
		seen:  map[string]time.Time{},
	}
}

// allow reports whether another request fits in the current minute.
// A non-positive limit accepts every request.
func (g *webhookGate) allow(now time.Time) bool {
	if g.limit <= 0 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.window.IsZero() || now.Sub(g.window) >= time.Minute {
		g.window = now
		g.used = 0
	}
	if g.used >= g.limit {
		return false
	}
	g.used++
	return true
}

// replay reports whether id was acknowledged within the TTL.
// An empty id is never a replay, so a delivery that omits the header
// keeps the previous behavior.
func (g *webhookGate) replay(id string, now time.Time) bool {
	if id == "" || g.ttl <= 0 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(now)
	_, ok := g.seen[id]
	return ok
}

// remember stores a delivery id that was handled successfully.
func (g *webhookGate) remember(id string, now time.Time) {
	if id == "" || g.ttl <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(now)
	g.seen[id] = now
}

// prune drops expired ids. The caller holds g.mu.
func (g *webhookGate) prune(now time.Time) {
	for id, at := range g.seen {
		if now.Sub(at) >= g.ttl {
			delete(g.seen, id)
		}
	}
}
