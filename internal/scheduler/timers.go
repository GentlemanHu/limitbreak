package scheduler

import (
	"context"
	"time"
)

// TimerClock is optional so existing Now-only manual clocks keep working.
// A timer must deliver at most once and Stop must prevent future deliveries.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}
type TimerClock interface {
	Clock
	NewTimer(time.Duration) Timer
}
type wallTimer struct{ timer *time.Timer }

func (t wallTimer) C() <-chan time.Time { return t.timer.C }
func (t wallTimer) Stop() bool          { return t.timer.Stop() }
func (e *Engine) newTimer(delay time.Duration) Timer {
	if delay < 0 {
		delay = 0
	}
	if clock, ok := e.clock.(TimerClock); ok {
		return clock.NewTimer(delay)
	}
	return wallTimer{time.NewTimer(delay)}
}

// Rescheduling is a notification, never an instruction to fetch immediately.
// In particular a completed Refresh cannot self-signal a polling busy loop.
func (e *Engine) reschedule() {
	select {
	case e.scheduleWake <- struct{}{}:
	default:
	}
}

func (e *Engine) refreshDeadline(ctx context.Context, now time.Time) time.Time {
	e.mu.Lock()
	last, interval := e.lastRefresh, e.pollInterval
	e.mu.Unlock()
	if last.IsZero() {
		return now
	}
	// A source-owned schedule is authoritative (including empty HTTP inventories
	// and failed sources). Dispatch account hints must not shorten its cadence.
	var due time.Time
	if source, ok := e.quota.(interface{ NextRefreshAt() time.Time }); ok {
		due = source.NextRefreshAt()
		if due.IsZero() {
			due = now
		}
	} else {
		due = last.Add(interval)
		if state, err := e.store.Read(ctx); err == nil {
			for _, account := range state.Accounts {
				if !account.NextCheckAt.IsZero() && account.NextCheckAt.Before(due) {
					due = account.NextCheckAt
				}
			}
		}
	}
	// CPA networking is separately guarded by Client; this bounds retries for
	// other adapters and store errors without changing their source deadlines.
	floor := last.Add(30 * time.Second)
	if last.After(now) {
		floor = now.Add(30 * time.Second)
	}
	if due.Before(floor) {
		due = floor
	}
	return due
}

func (e *Engine) runRefresh(ctx context.Context) {
	for ctx.Err() == nil {
		now := e.now()
		due := e.refreshDeadline(ctx, now)
		if !due.After(now) {
			if err := e.Refresh(ctx); err != nil && ctx.Err() == nil {
				_ = e.recordError(ctx, "quota_refresh", err)
			}
			continue
		}
		// A bounded watchdog reevaluates wall time after sleep/clock changes. It
		// only rearms the timer; no network request occurs until source due time.
		delay := due.Sub(now)
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := e.newTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-e.scheduleWake:
			timer.Stop()
		case <-timer.C():
		}
		// A backward clock jump invalidates the old observation schedule, not
		// Retry-After/authorization protection or any persisted lifecycle event.
		if current := e.now(); current.Before(now) {
			if source, ok := e.quota.(interface{ ClockChanged(context.Context) error }); ok {
				if err := source.ClockChanged(ctx); err != nil && ctx.Err() == nil {
					_ = e.recordError(ctx, "quota_clock_changed", err)
				}
			}
			e.mu.Lock()
			e.lastRefresh = current
			e.mu.Unlock()
		}
	}
}
