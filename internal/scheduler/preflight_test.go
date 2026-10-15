package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestSafePreflightFailuresWaitAndRecover(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result domain.BridgeResult
	}{
		{"locked", domain.BridgeResult{OK: true, AccessibilityGranted: true, ScreenLocked: true, Code: "screen_locked"}},
		{"accessibility", domain.BridgeResult{Code: "accessibility_denied"}},
		{"missing", domain.BridgeResult{Code: "bridge_missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, q, b := fixture(t)
			b.probeResult = &tc.result
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			scan(t, e)
			first, err := e.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state(t, e, "s").State != domain.Cooldown || first.Jobs[0].Attempts != 0 || b.count() != 0 {
				t.Fatal("safe preflight failure terminated or called Resume")
			}
			for i := 0; i < 5; i++ {
				scan(t, e)
			}
			b.mu.Lock()
			probes := b.probes
			b.mu.Unlock()
			if probes != 1 {
				t.Fatal("preflight delay ignored; probe repeated every scan")
			}
			// Simulate expiration of the persisted GUI probe delay, then grant access.
			b.mu.Lock()
			b.probeResult = nil
			b.mu.Unlock()
			if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
				for id, j := range tx.Jobs {
					j.NotBefore = time.Now().Add(-time.Second)
					tx.Jobs[id] = j
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			scan(t, e)
			if b.count() != 1 || state(t, e, "s").State != domain.Verifying {
				t.Fatal("safe preflight did not recover after delay")
			}
		})
	}
}
func TestAmbiguousPreflightRequiresAttention(t *testing.T) {
	e, q, b := fixture(t)
	b.probeResult = &domain.BridgeResult{Code: "ambiguous_target"}
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	scan(t, e)
	if b.count() != 0 || state(t, e, "s").State != domain.Attention {
		t.Fatal("identity ambiguity was automatically retried")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probes != 1 {
		t.Fatal("identity ambiguity reprobed")
	}
}
func TestUnknownQuotaFutureHintDoesNotDrift(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	a := allowed("a")
	a.State = "unknown"
	refresh(t, e, q, a)
	scan(t, e)
	before, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	after, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Jobs[0].NotBefore.Equal(before.Jobs[0].NotBefore) || b.count() != 0 {
		t.Fatal("unknown quota countdown drifted")
	}
	// Fresh real evidence may resume earlier than a conservative unknown hint.
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("fresh quota unnecessarily waited for the old unknown hint")
	}
}

func TestDirectPreflightSkipsScreenGatesAndKeepsDesktopState(t *testing.T) {
	e, q, b := fixture(t)
	b.probeResult = &domain.BridgeResult{OK: true, AccessibilityGranted: true, Code: "direct_ready", Channel: domain.ChannelDirect}
	b.result = domain.BridgeResult{OK: true, Submitted: true, AccessibilityGranted: true, Code: "direct_delivered", Channel: domain.ChannelDirect}
	// The last real desktop observation says the screen is locked and AX is
	// denied; neither matters for the direct channel nor may it be overwritten.
	if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
		tx.Capabilities.ScreenLocked = true
		tx.Capabilities.AccessibilityGranted = false
		tx.Capabilities.DesktopCode = "screen_locked"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	queueInMode(t, e, "s", "default")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if b.count() != 1 || state(t, e, "s").State != domain.Verifying {
		t.Fatalf("direct send not attempted: calls=%d state=%s", b.count(), state(t, e, "s").State)
	}
	b.mu.Lock()
	direct := b.bindings[0].ExecutionDirect
	b.mu.Unlock()
	if !direct {
		t.Fatal("default policy did not prefer the direct channel")
	}
	if got := state(t, e, "s").Reason; domain.Explain(got) == got {
		t.Fatalf("direct reason has no explanation: %q", got)
	}
	snap, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Capabilities.ScreenLocked || snap.Capabilities.AccessibilityGranted || snap.Capabilities.DesktopCode != "screen_locked" {
		t.Fatalf("direct probe overwrote desktop capabilities: %+v", snap.Capabilities)
	}
}

func TestWokenDeliveryIsRecordedWithAttentionSettings(t *testing.T) {
	e, q, b := fixture(t)
	b.probeResult = &domain.BridgeResult{OK: true, AccessibilityGranted: true, Code: "direct_wake_ready", Channel: domain.ChannelDirect}
	b.result = domain.BridgeResult{OK: true, Submitted: true, AccessibilityGranted: true, Code: "direct_delivered", Channel: domain.ChannelDirect, Woken: true}
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if b.count() != 1 || state(t, e, "s").State != domain.Verifying {
		t.Fatalf("woken send not attempted: calls=%d", b.count())
	}
	b.mu.Lock()
	sent := b.bindings[0]
	b.mu.Unlock()
	if !sent.ExecutionRestoreFocus || sent.ExecutionQuietSeconds != 120 {
		t.Fatalf("default attention settings not passed to the executor: restore=%v quiet=%d", sent.ExecutionRestoreFocus, sent.ExecutionQuietSeconds)
	}
	snap, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Jobs[0].Delivery != domain.DeliveryWokenDirect || !strings.Contains(domain.Explain(snap.Jobs[0].Reason), "唤醒") {
		t.Fatalf("woken delivery not recorded: %+v", snap.Jobs[0])
	}
}

func TestClaudeInUseIsRetriedNotEscalated(t *testing.T) {
	e, q, b := fixture(t)
	b.probeResult = &domain.BridgeResult{OK: true, AccessibilityGranted: true, Code: "direct_wake_ready", Channel: domain.ChannelDirect}
	b.result = domain.BridgeResult{Code: "claude_in_use", Message: "Claude 正在使用", AccessibilityGranted: true}
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	got := state(t, e, "s")
	if got.State == domain.Attention || !strings.HasPrefix(got.Reason, "waiting for desktop: claude_in_use") {
		t.Fatalf("claude_in_use escalated: %s %q", got.State, got.Reason)
	}
}

func TestBusySessionWaitsWithoutSending(t *testing.T) {
	e, q, b := fixture(t)
	b.probeResult = &domain.BridgeResult{AccessibilityGranted: true, Code: "session_busy", Channel: domain.ChannelDirect}
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	snap, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.count() != 0 || state(t, e, "s").State != domain.Cooldown || snap.Jobs[0].Attempts != 0 {
		t.Fatal("busy session was interrupted or the attempt consumed")
	}
}

// queueInMode queues a continue whose StopFailure hook reported mode.
func queueInMode(t *testing.T, e *Engine, id, mode string) {
	t.Helper()
	if err := e.Bind(context.Background(), binding(id)); err != nil {
		t.Fatal(err)
	}
	ev := domain.HookEvent{EventID: newID(), SessionID: id, HookEventName: "StopFailure", Error: "rate_limit_error", ObservedAt: time.Now().UTC(), PermissionMode: mode}
	if err := e.HandleEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

// Claude Code holds a cross-session message that asserts no sender mode when
// the receiver bypasses permissions, and a desktop session cannot approve it.
// Such sessions, and sessions whose mode is unknown, must get the UI channel.
func TestDirectChannelFollowsReportedPermissionMode(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		direct bool
	}{
		{"bypassPermissions", false},
		{"plan", false},
		{"", false},
		{"default", true},
		{"acceptEdits", true},
		{"auto", true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			e, q, b := fixture(t)
			queueInMode(t, e, "s", tc.mode)
			if got := state(t, e, "s").PermissionMode; got != tc.mode {
				t.Fatalf("permission mode not recorded: %q", got)
			}
			refresh(t, e, q, allowed("a"))
			scan(t, e)
			if b.count() != 1 {
				t.Fatalf("send not attempted: calls=%d", b.count())
			}
			b.mu.Lock()
			direct := b.bindings[0].ExecutionDirect
			b.mu.Unlock()
			if direct != tc.direct {
				t.Fatalf("mode %q: direct=%v, want %v", tc.mode, direct, tc.direct)
			}
		})
	}
}

// A later hook without a mode keeps the last reported one.
func TestPermissionModeSurvivesHooksWithoutIt(t *testing.T) {
	e, _, _ := fixture(t)
	queueInMode(t, e, "s", "bypassPermissions")
	hook(t, e, "s", "Stop", time.Now().UTC())
	if got := state(t, e, "s").PermissionMode; got != "bypassPermissions" {
		t.Fatalf("permission mode lost: %q", got)
	}
}
