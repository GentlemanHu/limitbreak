package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestPollingConfiguration(t *testing.T) {
	e, _, _ := fixture(t)
	if e.pollInterval != 180*time.Second {
		t.Fatal("unexpected default poll interval")
	}
	if err := e.SetPollInterval(10 * time.Second); err == nil {
		t.Fatal("unsafe HTTP polling interval accepted")
	}
	if err := e.SetPollInterval(240 * time.Second); err != nil {
		t.Fatal(err)
	}
	if e.pollInterval != 240*time.Second {
		t.Fatal("poll interval did not change")
	}
}
func TestPeerCancellationCancelsBridge(t *testing.T) {
	e, q, b := fixture(t)
	b.entered = make(chan struct{}, 1)
	b.release = make(chan struct{})
	b.cancelled = make(chan struct{}, 1)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.entered)
	peer := New(e.store, q, b, policy())
	if err := peer.Cancel(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	receive(t, b.cancelled)
	wg.Wait()
	if state(t, e, "s").State != domain.Cancelled {
		t.Fatal("peer cancellation was overwritten")
	}
}
func TestPausedPolicyCancelsPreflight(t *testing.T) {
	e, q, b := fixture(t)
	b.probeGate = make(chan struct{})
	b.probeEntered = make(chan struct{}, 1)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.probeEntered)
	p := policy()
	p.Paused = true
	if err := e.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	close(b.probeGate)
	wg.Wait()
	if b.count() != 0 || state(t, e, "s").State != domain.Attention {
		t.Fatal("paused preflight dispatched or failed to require review")
	}
}
func TestQuotaEvidenceRecheckedAfterPreflight(t *testing.T) {
	e, q, b := fixture(t)
	b.probeGate = make(chan struct{})
	b.probeEntered = make(chan struct{}, 1)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.probeEntered)
	a := allowed("a")
	a.State = "unknown"
	refresh(t, e, q, a)
	close(b.probeGate)
	wg.Wait()
	if b.count() != 0 || state(t, e, "s").State != domain.Attention {
		t.Fatal("quota became unknown but bridge was still sent")
	}
}
func TestNewPromptCancelsQueuedGeneration(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	generation := state(t, e, "s").Generation
	hook(t, e, "s", "UserPromptSubmit", time.Now().UTC())
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	s := state(t, e, "s")
	snap, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Generation != generation+1 || s.State != domain.Running || b.count() != 0 || snap.Jobs[0].State != domain.Cancelled {
		t.Fatal("new user prompt did not supersede old queued generation")
	}
}
func TestUnavailableSelectedAccountDoesNotExpandPool(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	other := allowed("not-bound")
	refresh(t, e, q, other)
	scan(t, e)
	if b.count() != 0 {
		t.Fatal("scheduler guessed a new account pool")
	}
}
func TestSafetyMarginAndUnknownBlockedReset(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	p := policy()
	p.SafetySeconds = 300
	if err := e.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	a := allowed("a")
	recent := time.Now().Add(-time.Minute)
	a.AvailableAt = &recent
	refresh(t, e, q, a)
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("fresh explicit allowance unnecessarily waited for an old reset hint")
	}
	hook(t, e, "s", "StopFailure", time.Now().UTC())
	a.State = "limited"
	a.AvailableAt = nil
	a.Windows[0].Blocked = true
	a.Windows[0].ResetAt = nil
	refresh(t, e, q, a)
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("unknown blocked reset sent")
	}
}
func TestExpiredDispatchReleasesResourceButNotLiveDesktopLease(t *testing.T) {
	e, q, b := fixture(t)
	b.entered = make(chan struct{}, 1)
	b.release = make(chan struct{})
	b.cancelled = make(chan struct{}, 1)
	queue(t, e, "s1")
	queue(t, e, "s2")
	refresh(t, e, q, allowed("a"))
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.entered)
	if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
		for id, j := range tx.Jobs {
			if j.State == domain.Dispatching {
				past := time.Now().Add(-time.Second)
				j.LeaseUntil = &past
				tx.Jobs[id] = j
			}
		}
		tx.LastSend = time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.cancelled)
	wg.Wait()
	if state(t, e, "s1").State != domain.Attention || b.count() != 1 {
		t.Fatal("timed out bridge was resent or desktop operations overlapped")
	}
}
func TestInvalidQuotaSnapshotBlocksDispatch(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	q.mu.Lock()
	q.accounts = []domain.Account{allowed("a"), allowed("a")}
	q.mu.Unlock()
	if err := e.Refresh(context.Background()); err == nil {
		t.Fatal("duplicate account snapshot accepted")
	}
	scan(t, e)
	if b.count() != 0 {
		t.Fatal("invalid quota snapshot failed open")
	}
}

func TestPostModelSwitchCancelsBindingAndQueue(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	event := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "PostModelSwitch", NewModel: "sonnet", ObservedAt: time.Now().UTC()}
	if err := e.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	s := state(t, e, "s")
	if s.Binding != nil || s.Model != "sonnet" || s.State != domain.Attention {
		t.Fatal("official model switch did not revoke binding")
	}
	hook(t, e, "s", "StopFailure", time.Now().UTC())
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if b.count() != 0 {
		t.Fatal("old binding used after official model switch")
	}
}
func TestTerminalHooksStayTerminalDespiteModelMismatch(t *testing.T) {
	for _, name := range []string{"Stop", "SessionEnd"} {
		t.Run(name, func(t *testing.T) {
			e, _, _ := fixture(t)
			queue(t, e, "s")
			event := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: name, Model: "another-model", ObservedAt: time.Now().UTC()}
			if err := e.HandleEvent(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			want := domain.Completed
			if name == "SessionEnd" {
				want = domain.Cancelled
			}
			if state(t, e, "s").State != want {
				t.Fatal("model metadata overrode terminal hook")
			}
		})
	}
}
