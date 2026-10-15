package proxy

import (
	"context"
	"strings"
	"time"
)

// Explicit refresh expires probe cadence only; hard Retry-After and rejected-key guards remain.
func (c *Client) InvalidateQuotaCache(ctx context.Context) error {
	_, err := c.invalidate(ctx, func(probeCache) bool { return true })
	return err
}

// InvalidateProvider expires the probe cadence of every account whose
// provider matches (case-insensitively) and reports how many were expired. A
// reset signal uses it to query those accounts on the next refresh. The same
// Retry-After and manual-refresh floors as InvalidateQuotaCache apply.
func (c *Client) InvalidateProvider(ctx context.Context, provider string) (int, error) {
	provider = strings.TrimSpace(provider)
	return c.invalidate(ctx, func(entry probeCache) bool { return strings.EqualFold(entry.provider, provider) })
}

func (c *Client) invalidate(ctx context.Context, match func(probeCache) bool) (int, error) {
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	now := c.now()
	n := 0
	for id, entry := range c.cache {
		if !match(entry) {
			continue
		}
		n++
		if entry.quota.retryAt.After(now) {
			continue
		}
		// Repeated manual refreshes reuse an answer younger than the floor
		// instead of querying the provider's usage endpoint again.
		if floor := entry.probed.Add(manualRefreshFloor); !entry.probed.IsZero() && floor.After(now) {
			if floor.Before(entry.next) {
				entry.next = floor
			}
		} else {
			entry.next = time.Time{}
		}
		c.cache[id] = entry
	}
	c.publishSchedule(c.now().UTC(), false)
	return n, nil
}
