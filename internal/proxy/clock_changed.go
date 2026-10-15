package proxy

import (
	"context"
	"time"
)

// ClockChanged expires cadence after a backward wall-clock jump. Original
// evidence, reset anchors and both management/upstream protection remain intact.
func (c *Client) ClockChanged(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.now().UTC()
	for id, cached := range c.cache {
		cached.next = now.Add(30 * time.Second)
		if cached.quota.retryAt.After(cached.next) {
			cached.next = cached.quota.retryAt
		}
		c.cache[id] = cached
	}
	c.publishSchedule(now, true)
	return nil
}
