package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// wakingBackend is a desktop backend that can open a session in the background.
type wakingBackend struct {
	stubBackend
	wake string
}

func (w wakingBackend) Wake(context.Context, domain.Binding, string) domain.BridgeResult {
	*w.calls = append(*w.calls, "wake")
	return domain.BridgeResult{OK: w.wake == "woken", Code: w.wake}
}

// sleepyDirect reports unavailable until `after` preflights have been made.
type sleepyDirect struct {
	after  int
	probes *int
	calls  *[]string
}

func (s sleepyDirect) live() bool { return *s.probes >= s.after }
func (s sleepyDirect) Preflight(context.Context, domain.Binding) domain.BridgeResult {
	*s.probes++
	if s.live() {
		return domain.BridgeResult{OK: true, Code: "direct_ready", Channel: domain.ChannelDirect, AccessibilityGranted: true}
	}
	return domain.BridgeResult{Code: "direct_unavailable", Channel: domain.ChannelDirect}
}
func (s sleepyDirect) Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error) {
	*s.calls = append(*s.calls, "direct")
	if !s.live() {
		return domain.BridgeResult{Code: "direct_unavailable", Channel: domain.ChannelDirect}, nil
	}
	return domain.BridgeResult{OK: true, Submitted: true, Code: "direct_delivered", Channel: domain.ChannelDirect, AccessibilityGranted: true}, nil
}

func wakeRouter(wake string, after int, calls *[]string) *Router {
	probes := 0
	r := NewRouter(wakingBackend{stubBackend{"desktop", calls}, wake}, nil).WithDirect(sleepyDirect{after, &probes, calls})
	r.wakeWait, r.wakePoll = 200*time.Millisecond, 5*time.Millisecond
	return r
}

func hostedDesktop() domain.Binding {
	return domain.Binding{Kind: "code", Executor: domain.ExecutorDesktop, ExecutionDirect: true,
		DeepLink: "claude://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab"}
}

func TestRouterWakesSessionThenDeliversSilently(t *testing.T) {
	var calls []string
	r := wakeRouter("woken", 3, &calls)
	res, err := r.Resume(context.Background(), hostedDesktop(), "继续")
	if err != nil || res.Code != "direct_delivered" || !res.Woken || res.Channel != domain.ChannelDirect {
		t.Fatalf("woken session not delivered directly: %+v %v", res, err)
	}
	if got := strings.Join(calls, ","); got != "direct,wake,direct" {
		t.Fatalf("calls %s", got)
	}
}

func TestRouterWakePreflightNeedsNoAccessibility(t *testing.T) {
	var calls []string
	r := wakeRouter("woken", 1000, &calls)
	res := r.Preflight(context.Background(), hostedDesktop())
	if !res.OK || res.Code != "direct_wake_ready" || res.Channel != domain.ChannelDirect {
		t.Fatalf("wakeable session preflight: %+v", res)
	}
	// Without a deep link there is nothing to wake: the UI channel decides.
	plain := hostedDesktop()
	plain.DeepLink = ""
	if res := r.Preflight(context.Background(), plain); res.Code != "desktop-probe" {
		t.Fatalf("non-wakeable preflight: %+v", res)
	}
	// Disabling direct delivery disables waking too.
	off := hostedDesktop()
	off.ExecutionDirect = false
	if res, _ := r.Resume(context.Background(), off, "继续"); res.Code != "desktop" {
		t.Fatalf("woke with direct delivery off: %+v", res)
	}
}

func TestRouterWakeFallsBackOnlyWhenSessionNeverStarts(t *testing.T) {
	var calls []string
	r := wakeRouter("woken", 1000, &calls)
	res, _ := r.Resume(context.Background(), hostedDesktop(), "继续")
	if res.Code != "desktop" || !res.Woken {
		t.Fatalf("session that never started did not use the UI channel: %+v", res)
	}
	if calls[len(calls)-1] != "desktop" || calls[1] != "wake" {
		t.Fatalf("calls %v", calls)
	}
}

func TestRouterWakeRefusalIsRetriedNotFallenBack(t *testing.T) {
	for _, code := range []string{"user_active", "claude_in_use", "screen_locked"} {
		var calls []string
		res, _ := wakeRouter(code, 1000, &calls).Resume(context.Background(), hostedDesktop(), "继续")
		if res.OK || res.Code != code || res.Submitted {
			t.Fatalf("%s: %+v", code, res)
		}
		if strings.Join(calls, ",") != "direct,wake" {
			t.Fatalf("%s reached another channel: %v", code, calls)
		}
	}
	// A wake the App cannot perform at all leaves the decision to the UI channel.
	var calls []string
	if res, _ := wakeRouter("app_not_running", 1000, &calls).Resume(context.Background(), hostedDesktop(), "继续"); res.Code != "desktop" || res.Woken {
		t.Fatalf("failed wake: %+v", res)
	}
}
