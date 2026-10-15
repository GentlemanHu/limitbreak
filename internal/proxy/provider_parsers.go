package proxy

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func parseClaude(b []byte, now time.Time) (evidence, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return evidence{}, err
	}
	root := object(v)
	if root == nil {
		return evidence{}, errors.New("Claude 额度响应不是对象")
	}
	q := evidence{checked: now, source: "claude-oauth"}
	shared := domain.QuotaGroup{ID: "shared", Name: "共享额度", Scope: "account", CheckedAt: now}
	keys := make([]string, 0, len(root))
	for k := range root {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		m := object(root[key])
		if m == nil {
			continue
		}
		n, ok := number(m["utilization"])
		if !ok {
			continue
		}
		w := domain.Window{ID: key, Name: claudeWindowName(key), LimitSeconds: namedDuration(key), ResetAt: resetAt(m, now)}
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 100 {
			w.UsedPercent = &n
			w.Blocked = n >= 100
		}
		if key == "five_hour" || key == "seven_day" {
			shared.Windows = append(shared.Windows, w)
		} else {
			g := domain.QuotaGroup{ID: key, Name: claudeWindowName(key), Scope: "unknown", CheckedAt: now, Windows: []domain.Window{w}}
			g.State, g.AvailableAt = domain.WindowState(g.Windows)
			if family := claudeFamily(key); family != "" {
				// A per-family weekly limit (Opus, Sonnet) only stops that
				// family; the scheduler applies it to matching sessions.
				g.Scope = "models"
				g.Models = []string{family}
				q.groups = append(q.groups, g)
				continue
			}
			q.groups = append(q.groups, g)
		}
		q.windows = append(q.windows, w)
		q.blocked = q.blocked || w.Blocked
	}
	if len(shared.Windows) > 0 {
		shared.State, shared.AvailableAt = domain.WindowState(shared.Windows)
		q.groups = append([]domain.QuotaGroup{shared}, q.groups...)
	}
	if limits, ok := root["limits"].([]any); ok {
		for i, item := range limits {
			m := object(item)
			n, valid := number(m["percent"])
			if !valid || n < 0 || n > 100 {
				continue
			}
			model := object(object(m["scope"])["model"])
			label := text(model["display_name"])
			if label == "" {
				label = "模型限制 " + strconv.Itoa(i+1)
			}
			w := domain.Window{ID: text(m["kind"]), Name: label, UsedPercent: &n, Blocked: n >= 100, ResetAt: resetAt(m, now)}
			g := domain.QuotaGroup{ID: "scope:" + text(m["kind"]) + ":" + label, Name: label, Scope: "unknown", Windows: []domain.Window{w}, CheckedAt: now}
			g.State, g.AvailableAt = domain.WindowState(g.Windows)
			if id := text(model["id"]); id != "" {
				g.Scope = "models"
				g.Models = []string{id}
				q.groups = append(q.groups, g)
				continue
			}
			q.groups = append(q.groups, g)
			q.windows = append(q.windows, w)
			q.blocked = q.blocked || w.Blocked
		}
	}
	for _, key := range []string{"extra_usage", "iguana_necktie"} {
		m := object(root[key])
		if m == nil {
			continue
		}
		for _, field := range []string{"is_enabled", "monthly_limit", "used_credits", "utilization", "limit_dollars", "used_dollars", "remaining_dollars"} {
			if val, exists := m[field]; exists {
				q.metrics = append(q.metrics, domain.QuotaMetric{Key: key + ":" + field, Label: metricName(field), Value: scalarText(val)})
			}
		}
	}
	q.allowed = !q.blocked && completeWindows(q.windows)
	if len(q.windows) == 0 {
		q.reason = "Claude 未返回可核验的额度窗口"
	}
	return q, nil
}

// claudeFamily names the model family a Claude usage key is scoped to.
func claudeFamily(key string) string {
	for _, family := range []string{"opus", "sonnet", "haiku"} {
		if strings.HasSuffix(key, "_"+family) {
			return family
		}
	}
	return ""
}

func claudeWindowName(key string) string {
	names := map[string]string{"five_hour": "5 小时额度", "seven_day": "7 天额度", "seven_day_opus": "Opus 周额度", "seven_day_sonnet": "Sonnet 周额度", "seven_day_oauth_apps": "OAuth 应用周额度", "iguana_necktie": "会话积分"}
	if n := names[key]; n != "" {
		return n
	}
	return key
}

func metricName(field string) string {
	names := map[string]string{"is_enabled": "额外用量已启用", "monthly_limit": "月度上限", "used_credits": "已用积分", "utilization": "使用率", "limit_dollars": "金额上限", "used_dollars": "已用金额", "remaining_dollars": "剩余金额", "monthlyLimit": "月度积分上限", "used": "月度已用", "onDemandCap": "按量付费上限", "onDemandUsed": "按量付费已用", "prepaidBalance": "预付余额"}
	if n := names[field]; n != "" {
		return n
	}
	return field
}

func cents(v any) (float64, bool) {
	if m := object(v); m != nil {
		v = m["val"]
	}
	return number(v)
}

func parseXAI(b []byte, now time.Time) (evidence, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return evidence{}, err
	}
	root := object(v)
	if root == nil {
		return evidence{}, errors.New("xAI billing 响应不是对象")
	}
	cfg := object(root["config"])
	if cfg == nil {
		return evidence{}, errors.New("xAI billing 响应缺少 config")
	}
	q := evidence{source: "xai-billing", checked: now}
	period := object(first(cfg, "currentPeriod", "current_period"))
	end := parseTime(first(period, "end"))
	if end.IsZero() {
		end = parseTime(first(cfg, "billingPeriodEnd", "billing_period_end"))
	}
	if n, ok := number(first(cfg, "creditUsagePercent", "credit_usage_percent")); ok && n >= 0 && n <= 100 {
		w := domain.Window{ID: "weekly", Name: "共享周额度", UsedPercent: &n, Blocked: n >= 100}
		if !end.IsZero() {
			w.ResetAt = &end
		}
		w.LimitSeconds = 604800
		g := domain.QuotaGroup{ID: "shared-weekly", Name: "共享周额度", Scope: "account", Windows: []domain.Window{w}, CheckedAt: now}
		g.State, g.AvailableAt = domain.WindowState(g.Windows)
		q.groups = append(q.groups, g)
		q.windows = append(q.windows, w)
		q.blocked = w.Blocked
	}
	if products, ok := first(cfg, "productUsage", "product_usage").([]any); ok {
		for _, item := range products {
			m := object(item)
			product := text(m["product"])
			if product == "" {
				continue
			}
			q.metrics = append(q.metrics, domain.QuotaMetric{Key: "product:" + product, Label: product + "（共享用量明细）", Value: scalarText(first(m, "usagePercent", "usage_percent")), Unit: "%"})
		}
	}
	for _, entry := range [][2]string{{"monthlyLimit", "monthly_limit"}, {"used", "used"}, {"onDemandCap", "on_demand_cap"}, {"onDemandUsed", "on_demand_used"}, {"prepaidBalance", "prepaid_balance"}} {
		if n, ok := cents(first(cfg, entry[0], entry[1])); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
			q.metrics = append(q.metrics, domain.QuotaMetric{Key: "billing:" + entry[0], Label: metricName(entry[0]), Value: strconv.FormatFloat(n/100, 'f', 2, 64), Format: "currency", Currency: "USD"})
		}
	}
	for _, entry := range [][2]string{{"billingPeriodStart", "billing_period_start"}, {"billingPeriodEnd", "billing_period_end"}} {
		if s := text(first(cfg, entry[0], entry[1])); s != "" {
			q.metrics = append(q.metrics, domain.QuotaMetric{Key: entry[0], Label: map[string]string{"billingPeriodStart": "月度周期开始", "billingPeriodEnd": "月度周期结束"}[entry[0]], Value: s})
		}
	}
	q.allowed = !q.blocked && completeWindows(q.windows)
	if len(q.windows) == 0 {
		q.reason = "此响应仅含财务指标，不单独授权恢复"
	}
	return q, nil
}
