package scheduler

import (
	"errors"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func validateResourceRefs(b domain.Binding) error {
	if b.MonitorMode != "" && b.MonitorMode != "any" && b.MonitorMode != "all" {
		return errors.New("监控组合必须为 any 或 all")
	}
	for _, refs := range [][]domain.ResourceRef{b.MonitorResources, b.ResumeResources} {
		if len(refs) > 256 {
			return errors.New("每个会话最多选择 256 个资源")
		}
		seen := map[domain.ResourceRef]bool{}
		for _, r := range refs {
			if r.AccountID == "" || len(r.AccountID) > 512 || len(r.GroupID) > 128 || len(r.WindowID) > 128 || seen[r] {
				return errors.New("资源引用无效或重复")
			}
			seen[r] = true
		}
	}
	for _, r := range b.ResumeResources {
		if r.WindowID != "" || !contains(b.AccountIDs, r.AccountID) {
			return errors.New("恢复来源必须是账号池内的完整账号或额度组，不能只选一个窗口")
		}
	}
	return nil
}

// A selected group is an alternative only after all applicable shared limits pass.
func scopedAccountReady(a domain.Account, b domain.Binding, p domain.Policy, now time.Time) bool {
	if a.Disabled || a.QuotaError != "" || (b.Provider != "" && a.Provider != b.Provider) || !contains(b.AccountIDs, a.ID) || !domain.ModelIn(a.Models, b.Model) {
		return false
	}
	if a.CheckedAt.IsZero() || a.CheckedAt.After(now) || now.Sub(a.CheckedAt) > time.Duration(p.FreshnessSeconds)*time.Second {
		return false
	}
	for _, ref := range b.ResumeResources {
		if ref.AccountID == a.ID && ref.GroupID == "" {
			legacy := b
			legacy.ResumeResources = nil
			return accountReady(a, legacy, p, now)
		}
	}
	selected := false
	for _, ref := range b.ResumeResources {
		if ref.AccountID != a.ID {
			continue
		}
		for _, g := range a.Groups {
			if g.ID != ref.GroupID {
				continue
			}
			if g.Scope != "account" && (g.Scope != "models" || !domain.GroupApplies(g, b.Model)) {
				continue
			}
			if groupReady(g, p, now) {
				selected = true
			}
		}
	}
	if !selected {
		return false
	}
	for _, g := range a.Groups {
		if domain.GroupApplies(g, b.Model) && !groupReady(g, p, now) {
			return false
		}
	}
	for _, w := range a.Windows {
		if w.Blocked && (w.Name == "credential status" || strings.HasPrefix(w.Name, "model:"+b.Model+":") || w.Name == "model:"+b.Model) {
			return false
		}
	}
	return true
}

func groupReady(g domain.QuotaGroup, p domain.Policy, now time.Time) bool {
	if g.State == "unknown" || g.CheckedAt.IsZero() || g.CheckedAt.After(now) || now.Sub(g.CheckedAt) > time.Duration(p.FreshnessSeconds)*time.Second {
		return false
	}
	state, _ := domain.WindowState(g.Windows)
	if state != "allowed" {
		return false
	}
	return true
}

func requiredGroup(g domain.QuotaGroup, a domain.Account, b domain.Binding) bool {
	if domain.GroupApplies(g, b.Model) {
		return true
	}
	for _, r := range b.ResumeResources {
		if r.AccountID == a.ID && (r.GroupID == "" || r.GroupID == g.ID) {
			return true
		}
	}
	return false
}

func evidenceAfterFailure(a domain.Account, b domain.Binding, failure time.Time) bool {
	if failure.IsZero() {
		return true
	}
	if !a.CheckedAt.After(failure) {
		return false
	}
	for _, g := range a.Groups {
		if (len(b.ResumeResources) == 0 || requiredGroup(g, a, b)) && !g.CheckedAt.After(failure) {
			return false
		}
	}
	return true
}

func accountResetHint(a domain.Account, b domain.Binding) *time.Time {
	if len(b.ResumeResources) == 0 {
		// The account-wide reset, or a later reset of a limited group that
		// applies to this session's model.
		hint := a.AvailableAt
		for _, g := range a.Groups {
			if g.State == "limited" && g.AvailableAt != nil && domain.GroupApplies(g, b.Model) && (hint == nil || g.AvailableAt.After(*hint)) {
				t := *g.AvailableAt
				hint = &t
			}
		}
		return hint
	}
	var windows []domain.Window
	for _, g := range a.Groups {
		if requiredGroup(g, a, b) {
			windows = append(windows, g.Windows...)
		}
	}
	_, at := domain.WindowState(windows)
	return at
}

// quotaEligible is the one sending gate shared by selection, the final
// pre-send check and hook confirmation. Fresh evidence after the failure
// passes; otherwise, when only the quota query is throttled, the recorded
// block's reset plus safety having passed allows one attempt (fallback=true).
func quotaEligible(a domain.Account, b domain.Binding, p domain.Policy, failure, now time.Time) (ok, fallback bool) {
	if accountReady(a, b, p, now) && evidenceAfterFailure(a, b, failure) {
		return true, false
	}
	if p.ResetFallback == domain.ResetFallbackOff || a.Disabled || a.State == "allowed" || !domain.QuotaQueryThrottled(a.QuotaError) {
		return false, false
	}
	_, passed := domain.ResetPassed(a, b.Model, failure, time.Duration(p.SafetySeconds)*time.Second, now)
	return passed, passed
}
