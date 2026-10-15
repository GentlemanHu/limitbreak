package adapters

import (
	"context"
	"errors"
	"time"
)

// ClockChanged rebases request deadlines only. Cached observations and health
// timestamps retain their original times and cannot become new sending evidence.
func (a *Aggregator) ClockChanged(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().UTC()
	failed := false
	for _, source := range a.instances {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if aware, ok := source.Quota.(interface{ ClockChanged(context.Context) error }); ok {
			if err := aware.ClockChanged(ctx); err != nil {
				failed = true
			}
		}
		interval := source.PollInterval
		if source.Type == "cliproxyapi" && (interval == 0 || interval > 30*time.Second) {
			interval = 30 * time.Second
		}
		if interval == 0 {
			a.nextDue[source.ID] = time.Time{}
		} else {
			a.nextDue[source.ID] = now.Add(interval)
		}
	}
	if failed {
		return errors.New("one or more quota sources could not rebase their request schedule")
	}
	return nil
}
