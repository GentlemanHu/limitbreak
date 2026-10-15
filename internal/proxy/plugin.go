package proxy

import (
	"errors"
	"math"
	"strconv"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// Summary values are display data, not positive authorization evidence.
func parsePlugin(b []byte, now time.Time) (evidence, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return evidence{}, err
	}
	root := object(v)
	if root == nil {
		return evidence{}, errors.New("quota plugin response is not an object")
	}
	q := evidence{checked: quotaTime(root, now), source: "management-quota"}
	if sub := object(root["subscription"]); sub != nil {
		q.plan = text(first(sub, "plan", "tierName", "tierId"))
	}
	if summary, ok := root["summary"].([]any); ok {
		for _, item := range summary {
			m := object(item)
			if m == nil {
				continue
			}
			q.metrics = append(q.metrics, domain.QuotaMetric{Key: text(m["key"]), Label: text(m["label"]), Value: scalarText(m["value"]), Unit: text(m["unit"]), Format: text(m["format"]), Currency: text(m["currency"])})
		}
	}
	groups, ok := root["groups"].([]any)
	if !ok || len(groups) == 0 {
		q.reason = "服务未返回额度分组"
		return q, nil
	}
	complete := true
	for gi, item := range groups {
		group := object(item)
		name := text(first(group, "displayName", "display_name"))
		if name == "" {
			name = "额度组 " + strconv.Itoa(gi+1)
		}
		g := domain.QuotaGroup{ID: name, Name: name, Description: text(group["description"]), Scope: "unknown", CheckedAt: q.checked, Windows: []domain.Window{}}
		buckets, ok := group["buckets"].([]any)
		if !ok || len(buckets) == 0 {
			complete = false
		}
		for bi, item := range buckets {
			bucket := object(item)
			window := text(bucket["window"])
			label := text(first(bucket, "displayName", "display_name", "description"))
			if label == "" {
				label = window
			}
			if label == "" {
				label = "窗口 " + strconv.Itoa(bi+1)
			}
			id := text(first(bucket, "bucketId", "bucket_id"))
			if id == "" {
				id = window
			}
			if id == "" {
				id = label
			}
			w := domain.Window{ID: id, Name: label, Description: text(bucket["description"])}
			if t := parseTime(first(bucket, "resetTime", "reset_time")); !t.IsZero() {
				w.ResetAt = &t
			}
			switch window {
			case "5h", "five-hour", "five_hour":
				w.LimitSeconds = 18000
			case "weekly", "week":
				w.LimitSeconds = 604800
			}
			fraction, valid := number(first(bucket, "remainingFraction", "remaining_fraction"))
			if valid && !math.IsNaN(fraction) && !math.IsInf(fraction, 0) && fraction >= 0 && fraction <= 1 {
				used := (1 - fraction) * 100
				w.UsedPercent = &used
				w.Blocked = fraction == 0
			} else {
				complete = false
			}
			q.blocked = q.blocked || w.Blocked
			g.Windows = append(g.Windows, w)
			flat := w
			flat.Name = name + ":" + label
			q.windows = append(q.windows, flat)
		}
		g.State, g.AvailableAt = domain.WindowState(g.Windows)
		q.groups = append(q.groups, g)
	}
	q.allowed = complete && len(q.windows) > 0 && !q.blocked
	if q.blocked {
		q.reason = "至少一个额度组已触顶；其他组独立显示"
	}
	if !complete {
		q.reason = "部分窗口缺少可信用量"
	}
	return q, nil
}

func scalarText(v any) string {
	if s := text(v); s != "" {
		if len(s) > 512 {
			return s[:512]
		}
		return s
	}
	if n, ok := number(v); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	if b, ok := boolean(v); ok {
		return strconv.FormatBool(b)
	}
	return ""
}

func completeWindows(windows []domain.Window) bool {
	if len(windows) == 0 {
		return false
	}
	for _, w := range windows {
		if w.Blocked || w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent >= 100 {
			return false
		}
	}
	return true
}
