package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestResumeOwnershipNeverInferred(t *testing.T) {
	for _, owner := range []string{"", "unknown", "native", "manual"} {
		t.Run("owner="+owner, func(t *testing.T) {
			e, q, bridge := fixture(t)
			b := binding("s")
			b.ResumeOwner = owner
			if err := e.Bind(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			hook(t, e, "s", "StopFailure", time.Now().UTC())
			refresh(t, e, q, allowed("a"))
			scan(t, e)
			scan(t, e)
			bridge.mu.Lock()
			probes := bridge.probes
			bridge.mu.Unlock()
			// Native/manual stay monitor-only; unresolved owners fall back to
			// hosting, which never sends before explicit automation consent.
			want := reasonNoConsent
			if owner == "native" || owner == "manual" {
				want = reasonLegacyNative
			}
			if bridge.count() != 0 || probes != 0 || !strings.Contains(state(t, e, "s").Reason, want) {
				t.Fatal("unresolved/native/manual ownership reached desktop execution")
			}
		})
	}
	e, q, bridge := fixture(t)
	queue(t, e, "s")
	// Exercise persisted upgraded state without weakening the public validator.
	if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
		s := tx.Sessions["s"]
		s.Binding.NativeAutoResumeDisabledAcknowledged = false
		tx.Sessions[s.ID] = s
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if bridge.count() != 0 {
		t.Fatal("missing native-disable acknowledgement authorized a persisted binding")
	}
}

func TestSessionContinueTextAndExecutionEnvelope(t *testing.T) {
	e, q, bridge := fixture(t)
	p := policy()
	p.UserIdleSeconds = 0 // Older policies use the effective default, not zero.
	if err := e.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	custom := "核验已完成的工作后，仅继续本会话的下一步。"
	for _, id := range []string{"custom", "default"} {
		b := binding(id)
		if id == "custom" {
			b.ContinueMessage = custom
		}
		if err := e.Bind(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		hook(t, e, id, "StopFailure", time.Now().UTC())
	}
	refresh(t, e, q, allowed("a"))
	for _, id := range []string{"custom", "default"} {
		generation := state(t, e, id).Generation
		resetSendClock(t, e)
		scan(t, e)
		bridge.mu.Lock()
		got := bridge.bindings[len(bridge.bindings)-1]
		message := bridge.messages[len(bridge.messages)-1]
		bridge.mu.Unlock()
		want := p.ContinueMessage
		if id == "custom" {
			want = custom
		}
		if message != want {
			t.Fatalf("session %s sent unexpected text: got %q, want %q", id, message, want)
		}
		if got.SessionID != id || got.ContinueMessage != want || got.ExecutionGeneration != generation || got.ExecutionLease == "" || got.ExecutionUserIdleSeconds != config.EffectivePolicy(p).UserIdleSeconds {
			t.Fatalf("incorrect session execution envelope: %+v", got)
		}
		hook(t, e, id, "Stop", time.Now().UTC())
	}
}

func TestAgingDispatchOutranksFreshHighPriorityAndPreservesFIFO(t *testing.T) {
	for _, equal := range []bool{false, true} {
		name := "outranks"
		if equal {
			name = "equal-priority-fifo"
		}
		t.Run(name, func(t *testing.T) {
			e, q, bridge := fixture(t)
			clock := &manualClock{at: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}
			e.clock = clock
			for _, id := range []string{"old", "new"} {
				b := binding(id)
				b.Priority = -100
				if id == "new" {
					b.Priority = 100
				}
				if err := e.Bind(context.Background(), b); err != nil {
					t.Fatal(err)
				}
				hook(t, e, id, "StopFailure", clock.Now())
			}
			periods := 201
			if equal {
				periods = 200
			}
			if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
				for id, job := range tx.Jobs {
					if job.SessionID == "old" {
						job.CreatedAt = clock.Now().Add(-time.Duration(periods*policy().QueueAgingSeconds) * time.Second)
						tx.Jobs[id] = job
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			clock.at = clock.at.Add(time.Second)
			a := allowed("a")
			a.CheckedAt = clock.Now()
			refresh(t, e, q, a)
			scan(t, e)
			bridge.mu.Lock()
			defer bridge.mu.Unlock()
			if bridge.calls != 1 || bridge.bindings[0].SessionID != "old" {
				t.Fatal("old low-priority job starved behind a fresh high-priority job")
			}
		})
	}
}

func TestStaleRunningRetainsGlobalAndAccountBudgetUntilConfirmedStop(t *testing.T) {
	for _, account := range []string{"a", "b"} {
		t.Run("queued-account="+account, func(t *testing.T) {
			e, q, bridge := fixture(t)
			clock := &manualClock{at: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}
			e.clock = clock
			p := policy()
			p.RunningStaleSeconds = 60
			if account == "b" {
				p.MaxRunning, p.MaxPerAccount = 1, 1
			}
			if err := e.SetPolicy(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if err := e.Bind(context.Background(), binding("running")); err != nil {
				t.Fatal(err)
			}
			hook(t, e, "running", "UserPromptSubmit", clock.Now())
			clock.at = clock.at.Add(61 * time.Second)
			b := binding("queued")
			b.AccountIDs = []string{account}
			if err := e.Bind(context.Background(), b); err != nil {
				t.Fatal(err)
			}
			hook(t, e, "queued", "StopFailure", clock.Now())
			clock.at = clock.at.Add(time.Second)
			a, bb := allowed("a"), allowed("b")
			a.CheckedAt, bb.CheckedAt = clock.Now(), clock.Now()
			refresh(t, e, q, a, bb)
			scan(t, e)
			scan(t, e)
			if bridge.count() != 0 || state(t, e, "running").State != domain.Attention {
				t.Fatal("missing RUNNING events freed a resource reservation")
			}
			stored, err := e.store.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := stored.RunningSince["running"]; !ok {
				t.Fatal("stale RUNNING marker was erased")
			}
			hook(t, e, "running", "Stop", clock.Now())
			scan(t, e)
			if bridge.count() != 1 {
				t.Fatal("confirmed stop did not release retained budget")
			}
		})
	}
}

func TestHourlyResumeBreakerSurvivesClockRollbackAndExpires(t *testing.T) {
	e, q, bridge := fixture(t)
	clock := &manualClock{at: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}
	e.clock = clock
	p := policy()
	p.MaxAutoResumesPerHour = 2
	if err := e.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := e.Bind(context.Background(), binding("s")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		hook(t, e, "s", "StopFailure", clock.Now())
		clock.at = clock.at.Add(time.Second)
		a := allowed("a")
		a.CheckedAt = clock.Now()
		refresh(t, e, q, a)
		scan(t, e)
		clock.at = clock.at.Add(31 * time.Second)
	}
	if bridge.count() != 2 || state(t, e, "s").State != domain.Attention {
		t.Fatal("hourly circuit breaker failed to block the third actual send")
	}
	before, err := e.store.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// History in the future after rollback still consumes budget. Do not forge
	// a future hook just to bypass the real event ordering watermark.
	clock.at = clock.at.Add(-time.Hour)
	if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
		if !resumeLimitReached(tx, "s", clock.Now()) || len(tx.ResumeHistory["s"]) != 2 {
			t.Error("clock rollback reset the resume breaker")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock.at = before.ResumeHistory["s"][1].Add(time.Hour + time.Second)
	if err := e.Bind(context.Background(), binding("s")); err != nil {
		t.Fatal(err)
	}
	hook(t, e, "s", "StopFailure", clock.Now())
	clock.at = clock.at.Add(time.Second)
	a := allowed("a")
	a.CheckedAt = clock.Now()
	refresh(t, e, q, a)
	scan(t, e)
	if bridge.count() != 3 {
		t.Fatal("expired hourly history never released the breaker")
	}
}

func TestUncertainResumeConsumesHourlySlotButSafePreflightDoesNot(t *testing.T) {
	for _, preflight := range []bool{false, true} {
		name := "uncertain-send"
		if preflight {
			name = "safe-preflight"
		}
		t.Run(name, func(t *testing.T) {
			e, q, bridge := fixture(t)
			clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
			e.clock = clock
			p := policy()
			p.MaxAutoResumesPerHour = 1
			if err := e.SetPolicy(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if err := e.Bind(context.Background(), binding("s")); err != nil {
				t.Fatal(err)
			}
			hook(t, e, "s", "StopFailure", clock.Now())
			clock.at = clock.at.Add(time.Second)
			if preflight {
				bridge.probeResult = &domain.BridgeResult{OK: true, AccessibilityGranted: true, ScreenLocked: true, Code: "screen_locked"}
			} else {
				bridge.result = domain.BridgeResult{Code: "outcome_unknown"}
			}
			a := allowed("a")
			a.CheckedAt = clock.Now()
			refresh(t, e, q, a)
			scan(t, e)
			stored, err := e.store.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wantHistory := 1
			if preflight {
				wantHistory = 0
			}
			if len(stored.ResumeHistory["s"]) != wantHistory {
				t.Fatal("hourly accounting confused a side effect with a safe environmental preflight")
			}
			clock.at = clock.at.Add(31 * time.Second)
			bridge.probeResult = nil
			bridge.result = domain.BridgeResult{OK: true, Submitted: true}
			if !preflight {
				hook(t, e, "s", "StopFailure", clock.Now())
			}
			clock.at = clock.at.Add(time.Second)
			a.CheckedAt = clock.Now()
			refresh(t, e, q, a)
			scan(t, e)
			if bridge.count() != 1 || (!preflight && state(t, e, "s").State != domain.Attention) || (preflight && state(t, e, "s").State != domain.Verifying) {
				t.Fatal("uncertain send bypassed breaker or safe preflight consumed the only resume slot")
			}
		})
	}
}

func TestNativeNotificationCancelsPendingAndInFlightGuardResume(t *testing.T) {
	for _, kind := range []string{"quota_auto_resume_fired", "quota_auto_resume_stale", "quota_auto_resume_disabled"} {
		for _, phase := range []string{"queued", "preflight", "resume"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				e, q, bridge := fixture(t)
				queue(t, e, "s")
				refresh(t, e, q, allowed("a"))
				var workers sync.WaitGroup
				if phase == "preflight" {
					bridge.probeEntered, bridge.probeGate = make(chan struct{}, 1), make(chan struct{})
				} else if phase == "resume" {
					bridge.entered, bridge.release, bridge.cancelled = make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
				}
				if phase != "queued" {
					if err := e.scan(context.Background(), &workers); err != nil {
						t.Fatal(err)
					}
					if phase == "preflight" {
						receive(t, bridge.probeEntered)
					} else {
						receive(t, bridge.entered)
					}
				}
				generation := state(t, e, "s").Generation
				event := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "Notification", NotificationType: kind, ObservedAt: time.Now().UTC()}
				if err := e.HandleEvent(context.Background(), event); err != nil {
					t.Fatal(err)
				}
				if phase == "preflight" {
					close(bridge.probeGate)
				} else if phase == "resume" {
					receive(t, bridge.cancelled)
				}
				workers.Wait()
				if err := e.HandleEvent(context.Background(), event); err != nil {
					t.Fatal(err)
				}
				scan(t, e)
				s := state(t, e, "s")
				wantState, wantCalls := domain.Attention, 0
				if kind == "quota_auto_resume_fired" {
					wantState = domain.Running
				}
				if phase == "resume" {
					wantCalls = 1 // An entered side effect cannot be retracted.
				}
				if bridge.count() != wantCalls || s.Generation != generation+1 || s.State != wantState || s.Binding.AutoResume || s.Binding.NativeAutoResumeDisabledAcknowledged {
					t.Fatal("native notification raced into external takeover or duplicate send")
				}
				stored, err := e.store.Read(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if kind == "quota_auto_resume_fired" {
					if s.Binding.ResumeOwner != "native" || stored.RunningSince["s"].IsZero() || len(stored.ResumeHistory["s"]) != wantCalls+1 {
						t.Fatal("native send lost ownership, lifecycle reservation or hourly accounting")
					}
				}
				for _, job := range stored.Jobs {
					if job.State == domain.Dispatching || job.State == domain.Verifying || job.State == domain.Ready || job.State == domain.Cooldown {
						t.Fatal("native notification left an external job armed")
					}
				}
			})
		}
	}
}
