package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func resetSendClock(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.store.Update(context.Background(), func(tx *store.Tx) error { tx.LastSend = time.Time{}; return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestDesktopSerialAndResourceConcurrency(t *testing.T) {
	e, q, b := fixture(t)
	b.entered = make(chan struct{}, 4)
	b.release = make(chan struct{})
	for _, id := range []string{"s1", "s2", "s3"} {
		bb := binding(id)
		if id == "s2" {
			bb.AccountIDs = []string{"b"}
		}
		if err := e.Bind(context.Background(), bb); err != nil {
			t.Fatal(err)
		}
		hook(t, e, id, "StopFailure", time.Now().UTC())
	}
	refresh(t, e, q, allowed("a"), allowed("b"))
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.entered)
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	if b.count() != 1 {
		t.Fatal("desktop operations ran concurrently")
	}
	close(b.release)
	wg.Wait()
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 2 {
		t.Fatal("different account did not use second resource slot")
	}
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 2 {
		t.Fatal("global resource limit exceeded")
	}
	// Release the first account by a real hook. No synthetic RUNNING state is used.
	hook(t, e, "s1", "Stop", time.Now().UTC())
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 3 {
		t.Fatal("normal Stop did not release resource pool capacity")
	}
}
func TestPerAccountLimitDespiteGlobalCapacity(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s1")
	queue(t, e, "s2")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("per-account limit exceeded")
	}
	hook(t, e, "s1", "UserPromptSubmit", time.Now().UTC())
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("confirmed RUNNING session lost its account reservation")
	}
	hook(t, e, "s1", "Stop", time.Now().UTC())
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 2 {
		t.Fatal("account reservation not released")
	}
}
func TestMinimumSendInterval(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s1")
	bb := binding("s2")
	bb.AccountIDs = []string{"b"}
	if err := e.Bind(context.Background(), bb); err != nil {
		t.Fatal(err)
	}
	hook(t, e, "s2", "StopFailure", time.Now().UTC())
	refresh(t, e, q, allowed("a"), allowed("b"))
	scan(t, e)
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("minimum send interval ignored")
	}
	resetSendClock(t, e)
	scan(t, e)
	if b.count() != 2 {
		t.Fatal("second account never dispatched")
	}
}
func TestAtomicDesktopLeaseAcrossEngines(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s1")
	queue(t, e, "s2")
	refresh(t, e, q, allowed("a"))
	b.entered = make(chan struct{}, 2)
	b.release = make(chan struct{})
	peer := New(e.store, q, b, policy())
	var workers sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	var callers sync.WaitGroup
	for _, engine := range []*Engine{e, peer} {
		callers.Add(1)
		go func(engine *Engine) {
			defer callers.Done()
			<-start
			errs <- engine.scan(context.Background(), &workers)
		}(engine)
	}
	close(start)
	callers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	receive(t, b.entered)
	if b.count() != 1 {
		t.Fatal("shared atomic lease allowed double send")
	}
	close(b.release)
	workers.Wait()
}
func TestRestartDoesNotResendUnconfirmed(t *testing.T) {
	for _, jobState := range []string{domain.Dispatching, domain.Verifying} {
		t.Run(jobState, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "restart.sqlite")
			s, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			q := &fixtureQuota{}
			b := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}
			e := New(s, q, b, policy())
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			if err = s.Update(context.Background(), func(tx *store.Tx) error {
				session := tx.Sessions["s"]
				session.State = jobState
				tx.Sessions["s"] = session
				for id, j := range tx.Jobs {
					j.State = jobState
					j.Attempts = 1
					j.AccountID = "a"
					expired := time.Now().Add(-time.Second)
					j.LeaseUntil = &expired
					tx.Jobs[id] = j
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			restarted := New(s, q, b, policy())
			scan(t, restarted)
			scan(t, restarted)
			if b.count() != 0 || state(t, restarted, "s").State != domain.Attention {
				t.Fatal("restart resent unconfirmed dispatched work")
			}
		})
	}
}
func TestCancelledQueueRemainsCancelledAfterRestart(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	if err := e.Cancel(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	refresh(t, e, q, allowed("a"))
	peer := New(e.store, q, b, policy())
	scan(t, peer)
	if b.count() != 0 || state(t, peer, "s").State != domain.Cancelled {
		t.Fatal("cancelled queue resurrected")
	}
}
func TestRunTimerDispatchesWithoutBrowser(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	q.accounts = []domain.Account{allowed("a")}
	b.entered = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	receive(t, b.entered)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}
func TestCancellationBarrierRejectsQueuedOldHooks(t *testing.T) {
	e, _, _ := fixture(t)
	queue(t, e, "s")
	old := time.Now().UTC()
	if err := e.Cancel(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	hook(t, e, "s", "UserPromptSubmit", old)
	if state(t, e, "s").State != domain.Cancelled {
		t.Fatal("old hook resurrected a cancelled session")
	}
}

// Opening a desktop session by its deep link makes Claude spawn the session's
// process, whose SessionStart arrives while the continue is still being typed.
// That lifecycle event must not cancel the in-flight send.
func TestSessionStartDuringDispatchKeepsSend(t *testing.T) {
	for _, source := range []string{"resume", "startup", ""} {
		t.Run("source="+source, func(t *testing.T) {
			e, q, b := fixture(t)
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			b.entered = make(chan struct{}, 1)
			b.release = make(chan struct{})
			b.cancelled = make(chan struct{}, 1)
			var workers sync.WaitGroup
			if err := e.scan(context.Background(), &workers); err != nil {
				t.Fatal(err)
			}
			receive(t, b.entered)
			start := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "SessionStart", Source: source, ObservedAt: time.Now().UTC().Add(time.Millisecond)}
			if err := e.HandleEvent(context.Background(), start); err != nil {
				t.Fatal(err)
			}
			select {
			case <-b.cancelled:
				t.Fatal("SessionStart cancelled the in-flight send")
			case <-time.After(150 * time.Millisecond):
			}
			close(b.release)
			workers.Wait()
			jobs := jobsOf(t, e, "s")
			if len(jobs) != 1 || jobs[0].State != domain.Verifying {
				t.Fatalf("jobs after send %+v", jobs)
			}
			hook(t, e, "s", "UserPromptSubmit", time.Now().UTC().Add(2*time.Millisecond))
			if jobs := jobsOf(t, e, "s"); jobs[0].State != domain.Completed {
				t.Fatalf("prompt hook did not confirm %+v", jobs)
			}
		})
	}
}

// /clear starts a new conversation lifecycle: the pending continue belongs to
// the old one and is stopped.
func TestClearDuringDispatchCancelsSend(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	b.entered = make(chan struct{}, 1)
	b.release = make(chan struct{})
	b.cancelled = make(chan struct{}, 1)
	var workers sync.WaitGroup
	if err := e.scan(context.Background(), &workers); err != nil {
		t.Fatal(err)
	}
	receive(t, b.entered)
	clear := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "SessionStart", Source: "clear", ObservedAt: time.Now().UTC().Add(time.Millisecond)}
	if err := e.HandleEvent(context.Background(), clear); err != nil {
		t.Fatal(err)
	}
	receive(t, b.cancelled)
	workers.Wait()
	for _, j := range jobsOf(t, e, "s") {
		if j.State == domain.Dispatching || j.State == domain.Verifying {
			t.Fatalf("job survived /clear %+v", j)
		}
	}
}
