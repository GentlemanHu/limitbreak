package proxy

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

type evidence struct {
	allowed bool
	blocked bool
	windows []domain.Window
	models  []string
	plan    string
	checked time.Time
	reason  string
	failed  bool
	retryAt time.Time
	groups  []domain.QuotaGroup
	metrics []domain.QuotaMetric
	source  string
}

// resetAt prefers an absolute reset. Relative resets are meaningful only when
// tied to an evidence timestamp, never the time of a later polling cycle.
func resetAt(m map[string]any, anchor time.Time) *time.Time {
	for _, key := range []string{"resets_at", "reset_at", "next_retry_after"} {
		if t := parseTime(m[key]); !t.IsZero() {
			return &t
		}
	}
	for _, key := range []string{"resets_in_seconds", "reset_after_seconds"} {
		if seconds, ok := number(m[key]); ok && seconds >= 0 && seconds < 315360000 && !anchor.IsZero() {
			t := anchor.Add(time.Duration(seconds * float64(time.Second))).UTC()
			return &t
		}
	}
	return nil
}

func parseWindow(v any, anchor time.Time, name string, parentBlocked bool) (domain.Window, bool) {
	m := object(v)
	if m == nil {
		return domain.Window{}, false
	}
	w := domain.Window{Name: name, ResetAt: resetAt(m, anchor)}
	if n, ok := number(m["used_percent"]); ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
		w.UsedPercent = &n
		if n >= 100 {
			w.Blocked = true
		}
	}
	if n, ok := number(m["limit_window_seconds"]); ok && n > 0 && n < 315360000 {
		w.LimitSeconds = int64(n)
	}
	for _, key := range []string{"blocked", "limit_reached", "exceeded"} {
		if b, ok := boolean(m[key]); ok && b {
			w.Blocked = true
		}
	}
	if b, ok := boolean(m["allowed"]); ok && !b {
		w.Blocked = true
	}
	// An overall block is not assigned to every non-exhausted window. Its
	// unknown reset is represented separately if no window explains the block.
	if parentBlocked && w.UsedPercent == nil {
		w.Blocked = true
	}
	return w, true
}

func parseRateLimit(m map[string]any, anchor time.Time, name string) evidence {
	q := evidence{checked: anchor}
	q.allowed, _ = boolean(m["allowed"])
	if b, ok := boolean(m["allowed"]); ok && !b {
		q.blocked = true
	}
	for _, key := range []string{"limit_reached", "blocked", "exceeded"} {
		if b, ok := boolean(m[key]); ok && b {
			q.blocked = true
		}
	}
	for _, key := range []string{"primary_window", "secondary_window"} {
		if w, ok := parseWindow(m[key], anchor, name+":"+key, q.blocked); ok {
			q.windows = append(q.windows, w)
			q.blocked = q.blocked || w.Blocked
		}
	}
	if windows, ok := m["windows"].([]any); ok {
		for i, v := range windows {
			label := text(object(v)["name"])
			if label == "" {
				label = itoa(i)
			}
			if w, ok := parseWindow(v, anchor, name+":"+label, q.blocked); ok {
				q.windows = append(q.windows, w)
				q.blocked = q.blocked || w.Blocked
			}
		}
	}
	// Direct window-shaped declarative payloads may supply allowed + used_percent.
	if _, ok := m["used_percent"]; ok {
		if w, valid := parseWindow(m, anchor, name, q.blocked); valid {
			q.windows = append(q.windows, w)
			q.blocked = q.blocked || w.Blocked
		}
	}
	if q.blocked && !hasBlocked(q.windows) {
		q.windows = append(q.windows, domain.Window{Name: name, Blocked: true, ResetAt: resetAt(m, anchor)})
	}
	if q.blocked {
		q.allowed = false
		q.reason = "quota limit reached"
	}
	return q
}

func itoa(i int) string { return strconv.Itoa(i) }

func hasBlocked(windows []domain.Window) bool {
	for _, w := range windows {
		if w.Blocked {
			return true
		}
	}
	return false
}

// availability is the latest reset across ALL blocked windows. A blocked
// window without a reset makes the account's recovery time unknown.
func availability(windows []domain.Window) *time.Time {
	var latest *time.Time
	for _, w := range windows {
		if !w.Blocked {
			continue
		}
		if w.ResetAt == nil {
			return nil
		}
		if latest == nil || w.ResetAt.After(*latest) {
			t := *w.ResetAt
			latest = &t
		}
	}
	return latest
}

// stillBlocked reports whether an observed exhaustion has not yet reached a
// reset: a blocked window without a reset time, or one resetting later.
func stillBlocked(windows []domain.Window, now time.Time) bool {
	for _, w := range windows {
		if w.Blocked && (w.ResetAt == nil || w.ResetAt.After(now)) {
			return true
		}
	}
	return false
}

func combine(a, b evidence) evidence {
	a.blocked = a.blocked || b.blocked
	a.allowed = a.allowed || b.allowed
	a.failed = a.failed || b.failed
	a.windows = append(a.windows, b.windows...)
	a.groups = append(a.groups, b.groups...)
	a.metrics = append(a.metrics, b.metrics...)
	if b.source != "" {
		a.source = b.source
	}
	for _, m := range b.models {
		a.models = appendUnique(a.models, m)
	}
	if b.plan != "" {
		a.plan = b.plan
	}
	if b.checked.After(a.checked) {
		a.checked = b.checked
	}
	if b.reason != "" {
		a.reason = b.reason
	}
	if b.retryAt.After(a.retryAt) {
		a.retryAt = b.retryAt
	}
	if a.blocked {
		a.allowed = false
	}
	return a
}

func independentModelEvidence(negative evidence) evidence {
	q := evidence{checked: negative.checked}
	for _, w := range negative.windows {
		if strings.HasPrefix(w.Name, "model:") && w.Blocked {
			q.windows = append(q.windows, w)
			q.blocked = q.blocked || w.Blocked
		}
	}
	return q
}

func (c *Client) finish(e credential, live evidence, next, now time.Time) domain.Account {
	a := e.account
	for _, m := range live.models {
		a.Models = appendUnique(a.Models, m)
	}
	for _, q := range c.cfg.Queries {
		if strings.EqualFold(q.Provider, a.Provider) {
			for _, m := range q.Models {
				a.Models = appendUnique(a.Models, m)
			}
		}
	}
	sort.Strings(a.Models)
	if live.plan != "" {
		a.Plan = live.plan
	}
	q := live
	// Fresh global quota can prove top-level cooldown recovery, but cannot
	// prove recovery of an independent model quota. Keep model-state limits.
	if live.allowed && !live.blocked && !live.failed && completeWindows(live.windows) && c.fresh(live.checked, now) {
		q = combine(live, independentModelEvidence(e.negative))
	} else {
		q = combine(e.negative, live)
	}
	a.Windows = cloneWindows(q.windows)
	materializeGroups(&a, live, now)
	a.CheckedAt = live.checked
	if a.CheckedAt.IsZero() {
		a.CheckedAt = q.checked
	}
	a.NextCheckAt = next
	if a.NextCheckAt.IsZero() {
		a.NextCheckAt = now.Add(time.Duration(c.cfg.Policy.UnknownProbeSeconds) * time.Second)
	}
	a.State = "unknown"
	a.Reason = "no explicit, fresh quota allowance"
	if q.reason != "" {
		a.Reason = q.reason
	}
	if q.allowed && !q.failed && completeWindows(q.windows) && c.fresh(live.checked, now) {
		a.State = "allowed"
		a.Reason = "fresh quota explicitly allows requests"
	}
	if q.blocked {
		a.State = "limited"
		a.Reason = "quota blocked; recovery requires a live probe"
		a.AvailableAt = availability(q.windows)
	}
	if a.Disabled {
		a.State = "disabled"
		a.Reason = "credential disabled"
		a.AvailableAt = nil
	}
	return a
}
