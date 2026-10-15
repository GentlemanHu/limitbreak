package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// Wall time and monotonic timer elapsed time are intentionally separate: a
// backward wall jump must not silently stretch an already armed watchdog.
type runtimeClock struct {
	mu         sync.Mutex
	wall       time.Time
	elapsed    time.Duration
	timers     map[*runtimeTimer]bool
	registered chan *runtimeTimer
}

type runtimeTimer struct {
	clock   *runtimeClock
	channel chan time.Time
	due     time.Duration
	delay   time.Duration
	wall    time.Time
	active  bool
}

func newRuntimeClock() *runtimeClock {
	return &runtimeClock{wall: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), timers: map[*runtimeTimer]bool{}, registered: make(chan *runtimeTimer, 256)}
}
func (c *runtimeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall }
func (c *runtimeClock) NewTimer(delay time.Duration) Timer {
	c.mu.Lock()
	timer := &runtimeTimer{clock: c, channel: make(chan time.Time, 1), due: c.elapsed + delay, delay: delay, wall: c.wall, active: true}
	c.timers[timer] = true
	c.mu.Unlock()
	c.registered <- timer
	return timer
}
func (t *runtimeTimer) C() <-chan time.Time { return t.channel }
func (t *runtimeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.active
	t.active = false
	delete(t.clock.timers, t)
	return wasActive
}
func (c *runtimeClock) advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(duration)
	c.elapsed += duration
	for timer := range c.timers {
		if timer.due <= c.elapsed {
			timer.active = false
			delete(c.timers, timer)
			timer.channel <- c.wall
		}
	}
}
func (c *runtimeClock) jump(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(duration)
}
func awaitRuntimeTimer(t *testing.T, c *runtimeClock, delay time.Duration) *runtimeTimer {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case timer := <-c.registered:
			c.mu.Lock()
			matches := timer.active && timer.delay == delay && timer.wall.Equal(c.wall)
			c.mu.Unlock()
			if matches {
				return timer
			}
		case <-deadline.C:
			t.Fatalf("Run never armed the expected %v timer", delay)
		}
	}
}

// This source exposes scheduling changes explicitly so tests exercise Run's
// actual select/rearm loop rather than invoking refreshDeadline in isolation.
type runtimeQuota struct {
	mu            sync.Mutex
	clock         *runtimeClock
	due           time.Time
	reset         time.Time
	checked       time.Time
	refreshes     int
	invalidations int
	clockChanges  int
	calls         chan struct{}
	changed       chan struct{}
}

func newRuntimeQuota(clock *runtimeClock) *runtimeQuota {
	return &runtimeQuota{clock: clock, reset: clock.Now().Add(10 * time.Minute), calls: make(chan struct{}, 32), changed: make(chan struct{}, 8)}
}
func (q *runtimeQuota) Refresh(context.Context) ([]domain.Account, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.refreshes++
	q.checked = q.clock.Now()
	q.due = q.checked.Add(180 * time.Second)
	q.calls <- struct{}{}
	return nil, nil // Empty inventories still have a source-owned deadline.
}
func (q *runtimeQuota) Capabilities() domain.Capabilities {
	return domain.Capabilities{ProxyConnected: true}
}
func (q *runtimeQuota) NextRefreshAt() time.Time { q.mu.Lock(); defer q.mu.Unlock(); return q.due }
func (q *runtimeQuota) InvalidateQuotaCache(context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.invalidations++
	q.due = time.Time{}
	return nil
}
func (q *runtimeQuota) SetPolicy(_ context.Context, p domain.Policy) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.checked.IsZero() {
		q.due = q.checked.Add(time.Duration(p.EarlyProbeSeconds) * time.Second)
	}
	return nil
}
func (q *runtimeQuota) ClockChanged(context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.clockChanges++
	q.due = q.clock.Now().Add(30 * time.Second)
	q.changed <- struct{}{}
	return nil
}
func (q *runtimeQuota) counts() (int, int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.refreshes, q.invalidations, q.clockChanges
}
func startTimerRun(t *testing.T, e *Engine, c *runtimeClock, q *runtimeQuota) {
	t.Helper()
	e.clock, e.quota = c, q
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Run exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Run did not join its timer workers on cancellation")
		}
	})
	receive(t, q.calls)
	awaitRuntimeTimer(t, c, 30*time.Second)
}

func TestRunManualRefreshRearmsActualTimerWithoutBusyLoop(t *testing.T) {
	e, _, bridge := fixture(t)
	clock := newRuntimeClock()
	quota := newRuntimeQuota(clock)
	startTimerRun(t, e, clock, quota)
	clock.jump(20 * time.Second)
	if err := e.RefreshNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	receive(t, quota.calls)
	awaitRuntimeTimer(t, clock, 30*time.Second)
	if calls, invalidations, _ := quota.counts(); calls != 2 || invalidations != 1 {
		t.Fatalf("manual refresh counts: calls=%d invalidations=%d", calls, invalidations)
	}
	// The old timer's deadline was +30; it must have been stopped, not retained.
	clock.advance(10 * time.Second)
	if calls, _, _ := quota.counts(); calls != 2 {
		t.Fatal("old refresh timer fired after manual refresh rearmed the schedule")
	}
	clock.advance(20 * time.Second)
	awaitRuntimeTimer(t, clock, 30*time.Second)
	if calls, _, _ := quota.counts(); calls != 2 || bridge.count() != 0 {
		t.Fatal("a scheduling wake or watchdog caused an immediate fetch/send loop")
	}
	// Exercise five actual watchdog cycles until the new source deadline.
	for i := 0; i < 5; i++ {
		clock.advance(30 * time.Second)
		if i == 4 {
			receive(t, quota.calls)
		}
		awaitRuntimeTimer(t, clock, 30*time.Second)
	}
	if calls, _, _ := quota.counts(); calls != 3 {
		t.Fatal("manual refresh did not establish the next real polling deadline")
	}
}

func TestRunSetPolicyEarlyProbeShortensAnArmedTimer(t *testing.T) {
	e, _, _ := fixture(t)
	clock := newRuntimeClock()
	quota := newRuntimeQuota(clock)
	startTimerRun(t, e, clock, quota)
	clock.jump(10 * time.Second)
	p := policy()
	p.EarlyProbeSeconds = 30 // Source shortens its early read-only cadence to +30.
	if err := e.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// Engine's 30-second fetch floor yields +30, i.e. a new 20-second timer.
	awaitRuntimeTimer(t, clock, 20*time.Second)
	if calls, _, _ := quota.counts(); calls != 1 {
		t.Fatal("SetPolicy fetched synchronously instead of rescheduling")
	}
	clock.advance(19 * time.Second)
	if calls, _, _ := quota.counts(); calls != 1 {
		t.Fatal("early probe bypassed the refresh floor")
	}
	clock.advance(time.Second)
	receive(t, quota.calls)
	awaitRuntimeTimer(t, clock, 30*time.Second)
	if calls, _, _ := quota.counts(); calls != 2 {
		t.Fatal("Run failed to refresh at the shortened policy deadline")
	}
}

func TestRunBackwardJumpForwardsClockChangedAndPreservesEvidenceTime(t *testing.T) {
	e, _, bridge := fixture(t)
	clock := newRuntimeClock()
	quota := newRuntimeQuota(clock)
	startTimerRun(t, e, clock, quota)
	quota.mu.Lock()
	original := quota.checked
	quota.mu.Unlock()
	clock.jump(-time.Hour)
	clock.advance(30 * time.Second)
	receive(t, quota.changed)
	awaitRuntimeTimer(t, clock, 30*time.Second)
	quota.mu.Lock()
	checked := quota.checked
	quota.mu.Unlock()
	if calls, _, changes := quota.counts(); calls != 1 || changes != 1 || !checked.Equal(original) || bridge.count() != 0 {
		t.Fatal("rollback refreshed evidence, skipped source notification or dispatched")
	}
	clock.advance(30 * time.Second)
	receive(t, quota.calls)
	awaitRuntimeTimer(t, clock, 30*time.Second)
	if calls, _, _ := quota.counts(); calls != 2 {
		t.Fatal("backward jump left the source stuck at its old future deadline")
	}
}
