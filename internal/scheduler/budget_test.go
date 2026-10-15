package scheduler

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// pidEvent is a hosted hook event whose desktop origin records the Claude process.
func pidEvent(t *testing.T, e *Engine, id, name string, pid int, at time.Time) {
	t.Helper()
	origin := desktopOrigin(at)
	origin.ClaudePID = pid
	v := domain.HookEvent{EventID: newID(), SessionID: id, HookEventName: name, ObservedAt: at, Kind: "code", Model: "opus[1m]", Origin: origin}
	if name == "StopFailure" {
		v.Error = "rate_limit_error"
	}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func limitRunning(t *testing.T, e *Engine, n int) {
	t.Helper()
	ctx := context.Background()
	snap, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PatchPolicy(ctx, snap.PolicyDigest, map[string]json.RawMessage{"max_running": json.RawMessage(strconv.Itoa(n))}); err != nil {
		t.Fatal(err)
	}
}

func TestUserDrivenSessionsDoNotHoldHostedBudget(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	limitRunning(t, e, 1)
	// Two sessions the user is working in, one of them long past the stale limit.
	hostedEvent(t, e, "u1", "UserPromptSubmit", clock.at)
	hostedEvent(t, e, "u2", "UserPromptSubmit", clock.at)
	clock.at = clock.at.Add(time.Hour)
	hostedEvent(t, e, "s", "StopFailure", clock.at)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 1 || b.bindings[0].SessionID != "s" {
		t.Fatalf("user-driven sessions blocked a hosted continue, calls=%d", b.count())
	}
}

func TestGuardContinuedTurnHoldsBudgetUntilProcessEnds(t *testing.T) {
	const pid = 4242
	cases := []struct {
		name  string
		ended func(observed time.Time) ProcessLookup
	}{
		{"process gone", func(time.Time) ProcessLookup {
			return func(int) (time.Time, bool) { return time.Time{}, false }
		}},
		{"pid reused", func(observed time.Time) ProcessLookup {
			return func(int) (time.Time, bool) { return observed.Add(time.Minute), true }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, q, b, clock := hostedFixture(t, true)
			limitRunning(t, e, 1)
			// LimitBreak continues "r"; its prompt hook confirms the guard-owned turn.
			pidEvent(t, e, "r", "StopFailure", pid, clock.at)
			clock.at = clock.at.Add(time.Second)
			refresh(t, e, q, freshAccount("a", clock.at))
			scan(t, e)
			if b.count() != 1 {
				t.Fatalf("first continue not sent, calls=%d", b.count())
			}
			observed := clock.at.Add(time.Second)
			pidEvent(t, e, "r", "UserPromptSubmit", pid, observed)
			if state(t, e, "r").State != domain.Running {
				t.Fatal("guard continue not confirmed")
			}
			// A second hosted session waits for the slot and says so.
			clock.at = clock.at.Add(10 * time.Minute)
			hostedEvent(t, e, "s", "StopFailure", clock.at)
			clock.at = clock.at.Add(time.Second)
			refresh(t, e, q, freshAccount("a", clock.at))
			e.SetProcessLookup(func(int) (time.Time, bool) { return observed.Add(-time.Hour), true })
			scan(t, e)
			if b.count() != 1 || !strings.Contains(state(t, e, "s").Reason, "并发已满") {
				t.Fatalf("live guard turn must hold the budget visibly, calls=%d reason=%q", b.count(), state(t, e, "s").Reason)
			}
			e.SetProcessLookup(c.ended(observed))
			scan(t, e)
			if b.count() != 2 || b.bindings[1].SessionID != "s" {
				t.Fatalf("ended process did not release the budget, calls=%d", b.count())
			}
			if r := state(t, e, "r"); r.State != domain.Completed || r.Reason != reasonProcessEnded {
				t.Fatalf("ended session not settled: %s %q", r.State, r.Reason)
			}
		})
	}
}

func TestUnknownProcessKeepsBudget(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	limitRunning(t, e, 1)
	// No recorded PID: nothing proves the guard-continued turn ended.
	hostedEvent(t, e, "r", "StopFailure", clock.at)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	hostedEvent(t, e, "r", "UserPromptSubmit", clock.at.Add(time.Second))
	clock.at = clock.at.Add(10 * time.Minute)
	hostedEvent(t, e, "s", "StopFailure", clock.at)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	e.SetProcessLookup(func(int) (time.Time, bool) { return time.Time{}, false })
	scan(t, e)
	if b.count() != 1 {
		t.Fatalf("budget released without process evidence, calls=%d", b.count())
	}
}

func TestAccountConcurrencyNamesHolder(t *testing.T) {
	const pid = 4243
	e, q, b, clock := hostedFixture(t, true)
	pidEvent(t, e, "r", "StopFailure", pid, clock.at)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	observed := clock.at.Add(time.Second)
	pidEvent(t, e, "r", "UserPromptSubmit", pid, observed)
	e.SetProcessLookup(func(int) (time.Time, bool) { return observed.Add(-time.Hour), true })
	clock.at = clock.at.Add(10 * time.Minute)
	hostedEvent(t, e, "s", "StopFailure", clock.at)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 1 {
		t.Fatalf("per-account limit not enforced, calls=%d", b.count())
	}
	if reason := state(t, e, "s").Reason; !strings.HasPrefix(reason, "account concurrency limit: 1 per account, held by 「") {
		t.Fatalf("holder missing from reason: %q", reason)
	}
}
