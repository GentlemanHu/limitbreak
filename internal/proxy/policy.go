package proxy

import (
	"context"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

func (c *Client) SetPolicy(ctx context.Context, p domain.Policy) error {
	if err := config.ValidatePolicy(p); err != nil {
		return err
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	c.cfg.Policy = p
	now := c.now().UTC()
	for id, cached := range c.cache {
		// The cadence changes, but existing evidence time and reset anchors never do.
		next := c.nextProbe(cached.quota, now, cached.failures)
		if !cached.next.IsZero() && cached.next.Before(next) {
			next = cached.next
		}
		if next.Before(now) {
			next = now
		}
		cached.next = next
		c.cache[id] = cached
	}
	c.publishSchedule(now, false)
	return nil
}

// NextRefreshAt reads a published source deadline without waiting for an HTTP
// request or sharing the mutable probe cache with scheduler goroutines.
func (c *Client) NextRefreshAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nextRefreshAt
}

// publishSchedule requires the client gate. An empty inventory still owns a
// polling deadline; account wake-up hints cannot make it fetch continuously.
func (c *Client) publishSchedule(now time.Time, refreshed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if refreshed {
		c.lastRefreshAt = now
	}
	if c.lastRefreshAt.IsZero() {
		c.nextRefreshAt = time.Time{}
		return
	}
	due := c.lastRefreshAt.Add(time.Duration(c.cfg.PollSeconds) * time.Second)
	if c.authFailure != 0 {
		due = c.lastRefreshAt.Add(time.Duration(c.cfg.Policy.UnknownProbeSeconds) * time.Second)
	}
	for _, cached := range c.cache {
		if cached.next.IsZero() || cached.next.Before(due) {
			due = cached.next
		}
	}
	floor := c.lastRefreshAt.Add(30 * time.Second)
	if c.lastRefreshAt.After(now) {
		floor = now.Add(30 * time.Second)
	}
	if due.Before(floor) {
		due = floor
	}
	if c.retryAt.After(due) {
		due = c.retryAt
	}
	c.nextRefreshAt = due
}
