package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func executionHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func TestLiveExecutionLeaseRechecksOwnershipQuotaTextAndGeneration(t *testing.T) {
	for _, change := range []string{"none", "lease", "generation", "text", "owner", "acknowledgement", "quota", "source", "expired"} {
		t.Run(change, func(t *testing.T) {
			e, q, bridge := fixture(t)
			bridge.entered, bridge.release = make(chan struct{}, 1), make(chan struct{})
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			var workers sync.WaitGroup
			if err := e.scan(context.Background(), &workers); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { close(bridge.release); workers.Wait() })
			receive(t, bridge.entered)
			bridge.mu.Lock()
			envelope := bridge.bindings[0]
			bridge.mu.Unlock()
			lease, generation, digest := envelope.ExecutionLease, envelope.ExecutionGeneration, executionHash(envelope.ContinueMessage)
			switch change {
			case "lease":
				lease += "/not-owner"
			case "generation":
				generation++
			case "text":
				digest = executionHash("unapproved replacement")
			default:
				if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
					s := tx.Sessions["s"]
					switch change {
					case "owner":
						s.Binding.ResumeOwner = "native"
					case "acknowledgement":
						s.Binding.NativeAutoResumeDisabledAcknowledged = false
					case "quota":
						tx.Accounts[0].State = "unknown"
					case "source":
						tx.Capabilities.ProxyConnected = false
						for i := range tx.Capabilities.Sources {
							tx.Capabilities.Sources[i].Connected = false
						}
					case "expired":
						tx.DesktopUntil = e.now().Add(-time.Second)
					}
					tx.Sessions[s.ID] = s
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			valid, err := e.ValidateExecution(context.Background(), "s", generation, lease, digest)
			if err != nil || valid != (change == "none") {
				t.Fatalf("execution qualification %s: valid=%v err=%v", change, valid, err)
			}
		})
	}
}

func TestDesktopHeartbeatRetainsLeaseAndObservesPeerRevocation(t *testing.T) {
	e, q, bridge := fixture(t)
	clock := newRuntimeClock()
	e.clock = clock
	bridge.entered, bridge.release, bridge.cancelled = make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
	if err := e.Bind(context.Background(), binding("s")); err != nil {
		t.Fatal(err)
	}
	hook(t, e, "s", "StopFailure", clock.Now())
	clock.jump(time.Second)
	a := allowed("a")
	a.CheckedAt = clock.Now()
	refresh(t, e, q, a)
	var workers sync.WaitGroup
	if err := e.scan(context.Background(), &workers); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(bridge.release); workers.Wait() })
	receive(t, bridge.entered)
	before, err := e.store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.jump(2 * time.Second)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		current, err := e.store.Read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if current.DesktopUntil.After(before.DesktopUntil) {
			if current.DesktopOwner != before.DesktopOwner {
				t.Fatal("heartbeat changed lease identity")
			}
			break
		}
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("live bridge lease was not extended by the real heartbeat")
		}
	}
	peer := NewWithClock(e.store, q, bridge, policy(), clock)
	if err := peer.Cancel(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	receive(t, bridge.cancelled)
	workers.Wait()
	stored, err := e.store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state(t, e, "s").State != domain.Cancelled || stored.DesktopOwner != "" || !stored.DesktopUntil.IsZero() || bridge.count() != 1 {
		t.Fatal("peer revocation or settled bridge released/overwrote the wrong lease")
	}
}

func TestDispatchPersistsActualDesktopProbeEvenWhenGenerationCancelled(t *testing.T) {
	e, q, bridge := fixture(t)
	bridge.probeEntered, bridge.probeGate = make(chan struct{}, 1), make(chan struct{})
	bridge.probeResult = &domain.BridgeResult{OK: true, AccessibilityGranted: true, ScreenLocked: true, Code: "screen_locked"}
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	var workers sync.WaitGroup
	if err := e.scan(context.Background(), &workers); err != nil {
		t.Fatal(err)
	}
	receive(t, bridge.probeEntered)
	if err := e.Cancel(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	close(bridge.probeGate)
	workers.Wait()
	// The cancellable fixture reports cancelled if it observes cancellation
	// before the gate release. Either outcome must be recorded, not fabricated.
	snapshot, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caps := snapshot.Capabilities
	if !caps.BridgeChecked || caps.DesktopCheckedAt == nil || caps.DesktopCode == "" || (caps.DesktopCode != "cancelled" && caps.DesktopCode != "screen_locked") || state(t, e, "s").State != domain.Cancelled || bridge.count() != 0 {
		t.Fatal("superseded dispatch discarded its actual desktop observation")
	}
}
