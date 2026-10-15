package domain

import (
	"strings"
	"time"
)

// Reset fallback policy values; "" behaves as ResetFallbackAuto.
const (
	ResetFallbackAuto = "auto"
	ResetFallbackOff  = "off"
)

func ValidResetFallback(v string) bool {
	return v == "" || v == ResetFallbackAuto || v == ResetFallbackOff
}

// QuotaQueryThrottled reports whether a quota query failure only says that the
// query itself could not be answered right now (rate limited, server error or
// timeout), as opposed to a credential that is rejected.
func QuotaQueryThrottled(reason string) bool {
	r := strings.ToLower(reason)
	if strings.Contains(r, "http 401") || strings.Contains(r, "http 403") {
		return false
	}
	return r != "" && (strings.Contains(r, "http 429") || strings.Contains(r, "http 5") || strings.Contains(r, "timeout") || strings.Contains(r, "deadline"))
}

// ResetPassed looks at the blocks recorded for an account whose quota cannot be
// confirmed right now. It returns the latest reset among the blocks that apply
// to model and whether that reset, plus safety, has passed. A block is only
// counted when it explains the failure: its reset falls after the session's
// quota failure. A block with no reset time, or one that cannot be tied to the
// failure, yields nil so the caller keeps waiting for real evidence.
func ResetPassed(a Account, model string, failure time.Time, safety time.Duration, now time.Time) (*time.Time, bool) {
	var latest *time.Time
	consider := func(at *time.Time) bool {
		if at == nil {
			return false
		}
		if latest == nil || at.After(*latest) {
			t := *at
			latest = &t
		}
		return true
	}
	for _, w := range a.Windows {
		if !windowApplies(w.Name, model) {
			continue
		}
		full := w.Blocked || (w.UsedPercent != nil && *w.UsedPercent >= 100)
		if full && !consider(w.ResetAt) {
			return nil, false
		}
	}
	for _, g := range a.Groups {
		if g.State == "limited" && GroupApplies(g, model) && !consider(g.AvailableAt) {
			return nil, false
		}
	}
	if latest == nil || (!failure.IsZero() && !latest.After(failure)) {
		return nil, false
	}
	return latest, !now.Before(latest.Add(safety))
}

// windowApplies keeps account-wide windows and the windows of model itself.
// With no known model every recorded window must clear.
func windowApplies(name, model string) bool {
	if !strings.HasPrefix(name, "model:") || model == "" {
		return true
	}
	m := NormalizeModel(model)
	return name == "model:"+m || strings.HasPrefix(name, "model:"+m+":")
}
