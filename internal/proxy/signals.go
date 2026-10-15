package proxy

import (
	"math"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// Passive management quota.signals is a map of actual response-header strings.
// It is never sufficient positive evidence. Additional limit labels and credit
// balances are not guessed into a universal block or a model routing table.
func parsePassiveSignals(signals map[string]any, anchor time.Time) evidence {
	h := make(map[string]string, len(signals))
	for k, v := range signals {
		if s, ok := v.(string); ok {
			h[strings.ToLower(k)] = s
		}
	}
	q := evidence{checked: anchor}
	for _, window := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + window + "-"
		m := map[string]any{}
		if value, ok := h[prefix+"used-percent"]; ok {
			m["used_percent"] = value
		}
		if value, ok := h[prefix+"reset-at"]; ok {
			m["reset_at"] = value
		}
		if value, ok := h[prefix+"reset-after-seconds"]; ok {
			m["reset_after_seconds"] = value
		}
		if value, ok := h[prefix+"window-minutes"]; ok {
			if n, valid := number(value); valid {
				m["limit_window_seconds"] = n * 60
			}
		}
		if value, ok := h[prefix+"allowed"]; ok {
			if b, err := strconv.ParseBool(strings.ToLower(value)); err == nil {
				m["allowed"] = b
			}
		}
		if value, ok := h[prefix+"limit-reached"]; ok {
			if b, err := strconv.ParseBool(strings.ToLower(value)); err == nil {
				m["limit_reached"] = b
			}
		}
		if len(m) == 0 {
			continue
		}
		if w, ok := parseWindow(m, anchor, "observed codex:"+window, false); ok {
			q.windows = append(q.windows, w)
			q.blocked = q.blocked || w.Blocked
		}
	}
	for _, key := range []string{"x-codex-allowed", "x-codex-limit-reached"} {
		if value, ok := h[key]; ok {
			if b, err := strconv.ParseBool(strings.ToLower(value)); err == nil && ((key == "x-codex-allowed" && !b) || (key == "x-codex-limit-reached" && b)) {
				q.blocked = true
			}
		}
	}
	for _, window := range []string{"5h", "7d"} {
		prefix := "anthropic-ratelimit-unified-" + window + "-"
		status, hasStatus := h[prefix+"status"]
		usage, hasUsage := h[prefix+"utilization"]
		reset, hasReset := h[prefix+"reset"]
		if !hasStatus && !hasUsage && !hasReset {
			continue
		}
		w := domain.Window{Name: "observed anthropic:" + window}
		if t := parseTime(reset); !t.IsZero() {
			w.ResetAt = &t
		}
		if n, ok := number(usage); ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			used := n * 100
			w.UsedPercent = &used
			w.Blocked = used >= 100
		}
		if strings.EqualFold(status, "rejected") {
			w.Blocked = true
		}
		q.windows = append(q.windows, w)
		q.blocked = q.blocked || w.Blocked
	}
	if q.blocked && !hasBlocked(q.windows) {
		var reset *time.Time
		if value, ok := h["retry-after"]; ok {
			reset = parseRetryAfter(value, anchor)
		}
		q.windows = append(q.windows, domain.Window{Name: "observed quota", Blocked: true, ResetAt: reset})
	}
	if q.blocked {
		q.reason = "passive provider headers report a quota block"
	}
	return q
}

func cloneWindows(windows []domain.Window) []domain.Window {
	out := append([]domain.Window(nil), windows...)
	for i := range out {
		if out[i].UsedPercent != nil {
			n := *out[i].UsedPercent
			out[i].UsedPercent = &n
		}
		if out[i].ResetAt != nil {
			t := *out[i].ResetAt
			out[i].ResetAt = &t
		}
	}
	return out
}
