package scheduler

import (
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestGroupAppliesIsConservative(t *testing.T) {
	opus := domain.QuotaGroup{Scope: "models", Models: []string{"opus"}}
	for _, tc := range []struct {
		group domain.QuotaGroup
		model string
		want  bool
	}{
		{opus, "claude-opus-4-8[1m]", true},
		{opus, "claude-sonnet-4-5", false},
		{opus, "", true},
		{opus, "default", true},
		{domain.QuotaGroup{Scope: "models", Models: []string{"claude-opus-4-1"}}, "claude-opus-4-8", true},
		{domain.QuotaGroup{Scope: "models", Models: []string{"claude-opus-4-1"}}, "claude-haiku-4-5", false},
		{domain.QuotaGroup{Scope: "operation"}, "gpt-5-codex", false},
		{domain.QuotaGroup{Scope: "unknown"}, "claude-sonnet-4-5", true},
		{domain.QuotaGroup{Scope: "account"}, "claude-sonnet-4-5", true},
	} {
		if got := domain.GroupApplies(tc.group, tc.model); got != tc.want {
			t.Fatalf("GroupApplies(%+v, %q)=%v wanted %v", tc.group, tc.model, got, tc.want)
		}
	}
}

func TestFamilyWeeklyLimitOnlyStopsThatFamily(t *testing.T) {
	p := policy()
	now := time.Now().UTC()
	used, full := 20.0, 100.0
	reset := now.Add(72 * time.Hour)
	a := allowed("a")
	a.Models = []string{"claude-opus-4-8", "claude-sonnet-4-5"}
	a.CheckedAt = now
	a.Windows = []domain.Window{{Name: "5 小时额度", UsedPercent: &used}, {Name: "7 天额度", UsedPercent: &used}}
	a.Groups = []domain.QuotaGroup{
		{ID: "shared", Scope: "account", State: "allowed", CheckedAt: now, Windows: a.Windows},
		{ID: "seven_day_opus", Scope: "models", Models: []string{"opus"}, State: "limited", AvailableAt: &reset, CheckedAt: now, Windows: []domain.Window{{Name: "Opus 周额度", UsedPercent: &full, Blocked: true, ResetAt: &reset}}},
		{ID: "code-review", Scope: "operation", State: "limited", CheckedAt: now, Windows: []domain.Window{{Name: "code review quota", Blocked: true}}},
	}
	sonnet := binding("one")
	sonnet.Model = "claude-sonnet-4-5"
	if !accountReady(a, sonnet, p, now) {
		t.Fatal("an exhausted Opus weekly limit or review quota blocked a Sonnet session")
	}
	opus := binding("two")
	opus.Model = "claude-opus-4-8"
	if accountReady(a, opus, p, now) {
		t.Fatal("an Opus session was sent while its weekly family limit is exhausted")
	}
	if hint := accountResetHint(a, opus); hint == nil || !hint.Equal(reset) {
		t.Fatalf("Opus session reset hint %v, wanted the family reset %v", hint, reset)
	}
	if hint := accountResetHint(a, sonnet); hint != nil {
		t.Fatalf("Sonnet session inherited an unrelated reset %v", hint)
	}
	unknown := binding("three")
	unknown.Model = ""
	if accountReady(a, unknown, p, now) {
		t.Fatal("a session of unknown model ignored a model-scoped limit")
	}
}
