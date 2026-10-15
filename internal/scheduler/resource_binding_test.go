package scheduler

import (
	"context"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestMonitoringDoesNotOverwriteSessionOrSend(t *testing.T) {
	e, q, bridge := fixture(t)
	now := time.Now().UTC()
	event := domain.HookEvent{EventID: newID(), SessionID: "one", HookEventName: "SessionStart", SessionTitle: "actual title", Cwd: "/actual/project", Model: "actual-model", Kind: "code", ObservedAt: now}
	if err := e.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	refs := []domain.ResourceRef{{AccountID: "a"}, {AccountID: "b"}}
	b := domain.Binding{SessionID: "one", Kind: "chat", Provider: "unrelated-provider", Model: "other-model", TargetTitle: "not the title", MonitorResources: refs, MonitorMode: "all"}
	if err := e.Bind(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	refresh(t, e, q, allowed("a"), allowed("b"))
	got := state(t, e, "one")
	if got.Title != "actual title" || got.Model != "actual-model" || got.Kind != "code" || got.Provider != "" || got.Monitor == nil || got.Monitor.State != "allowed" {
		t.Fatalf("monitor changed metadata: %+v", got)
	}
	hook(t, e, "one", "StopFailure", time.Now().UTC())
	scan(t, e)
	if bridge.count() != 0 {
		t.Fatal("monitor-only binding sent a message")
	}
	b.SessionID = "two"
	if err := e.Bind(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(state(t, e, "two").Monitor.Resources) != 2 {
		t.Fatal("resource cannot be shared across sessions")
	}
	event.EventID = newID()
	event.HookEventName = "PostModelSwitch"
	event.NewModel = "changed-model"
	event.ObservedAt = time.Now().UTC()
	if err := e.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if got := state(t, e, "one"); got.Binding == nil || len(got.Binding.MonitorResources) != 2 || got.Binding.AutoResume {
		t.Fatal("model change lost monitors or retained automatic authorization")
	}
}

func TestResumeRequiresWholeGroupAndSharedConstraints(t *testing.T) {
	p := policy()
	now := time.Now().UTC()
	used, full := 15.0, 100.0
	a := allowed("a")
	a.CheckedAt = now
	a.State = "limited"
	a.Groups = []domain.QuotaGroup{
		{ID: "target", Scope: "models", Models: []string{"opus"}, CheckedAt: now, Windows: []domain.Window{{UsedPercent: &used}}},
		{ID: "other-model", Scope: "models", Models: []string{"unrelated"}, CheckedAt: now, Windows: []domain.Window{{UsedPercent: &full, Blocked: true}}},
		{ID: "shared", Scope: "account", CheckedAt: now, Windows: []domain.Window{{Name: "5h", UsedPercent: &used}, {Name: "weekly", UsedPercent: &used}}},
	}
	b := binding("one")
	b.ResumeResources = []domain.ResourceRef{{AccountID: "a", GroupID: "target"}}
	if !accountReady(a, b, p, now) {
		t.Fatal("unrelated model blocked known target scope")
	}
	a.Groups[2].Windows[1].UsedPercent = &full
	a.Groups[2].Windows[1].Blocked = true
	if accountReady(a, b, p, now) {
		t.Fatal("healthy 5h window bypassed shared weekly exhaustion")
	}
	b.ResumeResources[0].WindowID = "five-hour"
	if validateResourceRefs(b) == nil {
		t.Fatal("single-window sending authorization accepted")
	}
	b.ResumeResources[0].WindowID = ""
	a.Groups[0].Scope = "unknown"
	if accountReady(a, b, p, now) {
		t.Fatal("unknown model scope authorized sending")
	}
}
