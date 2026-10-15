package domain

import (
	"testing"
	"time"
)

func TestMonitorCombinationsAndSharedWindowRules(t *testing.T) {
	now := time.Now().UTC()
	low, full := 20.0, 100.0
	later := now.Add(7 * time.Hour)
	accounts := []Account{{ID: "a", Provider: "claude", CheckedAt: now, Groups: []QuotaGroup{{ID: "g", Name: "shared", CheckedAt: now, Windows: []Window{{ID: "short", UsedPercent: &low}, {ID: "week", UsedPercent: &full, Blocked: true, ResetAt: &later}}}}}, {ID: "b", Provider: "codex", CheckedAt: now, Windows: []Window{{UsedPercent: &low}}}}
	refs := []ResourceRef{{AccountID: "a", GroupID: "g"}, {AccountID: "b"}}
	any := ObserveResources(refs, "any", accounts, now, time.Minute)
	all := ObserveResources(refs, "all", accounts, now, time.Minute)
	if any.State != "allowed" || all.State != "limited" || all.AvailableAt == nil || !all.AvailableAt.Equal(later) {
		t.Fatalf("bad combination: %+v / %+v", any, all)
	}
	group := ObserveResources(refs[:1], "any", accounts, now, time.Minute)
	if group.State != "limited" {
		t.Fatal("healthy short window bypassed shared weekly limit")
	}
	single := ObserveResources([]ResourceRef{{AccountID: "a", GroupID: "g", WindowID: "short"}}, "any", accounts, now, time.Minute)
	if single.State != "allowed" {
		t.Fatal("single-window observation must remain independent from sending authorization")
	}
	if stale := ObserveResources(refs, "all", accounts, now.Add(2*time.Minute), time.Minute); stale.State == "allowed" {
		t.Fatal("stale observation allowed")
	}
}

func TestMissingAndDisabledResourcesRemainVisible(t *testing.T) {
	now := time.Now()
	refs := []ResourceRef{{AccountID: "gone"}, {AccountID: "disabled"}}
	status := ObserveResources(refs, "any", []Account{{ID: "disabled", Disabled: true, CheckedAt: now}}, now, time.Minute)
	if status.State != "unknown" || len(status.Resources) != 2 || status.Resources[0].State != "missing" || status.Resources[1].State != "disabled" {
		t.Fatalf("lost resource state: %+v", status)
	}
}

func TestUncertainResetDoesNotProduceAllReadyHint(t *testing.T) {
	now := time.Now()
	full := 100.0
	later := now.Add(time.Hour)
	accounts := []Account{{ID: "a", CheckedAt: now, Windows: []Window{{Blocked: true, UsedPercent: &full}}}, {ID: "b", CheckedAt: now, Windows: []Window{{Blocked: true, UsedPercent: &full, ResetAt: &later}}}}
	status := ObserveResources([]ResourceRef{{AccountID: "a"}, {AccountID: "b"}}, "all", accounts, now, time.Minute)
	if status.AvailableAt != nil {
		t.Fatal("unknown reset was replaced by another resource's reset")
	}
}
