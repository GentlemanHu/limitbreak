package proxy

import (
	"context"
	"errors"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

const codexResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// Credit availability is presentation-only. This never calls /consume or resets any quota.
func (c *Client) codexCreditMetrics(ctx context.Context, key, prefix string, e credential) []domain.QuotaMetric {
	c.mu.RLock()
	retry := c.creditRetry[e.account.ID]
	c.mu.RUnlock()
	if c.now().Before(retry) {
		return []domain.QuotaMetric{{Key: "reset-credits-unavailable", Label: "主动重置次数", Value: "等待上游允许复查"}}
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	b, err := c.apiCall(ctx, key, prefix, e, config.QuotaQuery{URL: codexResetCreditsURL, Headers: map[string]string{"Chatgpt-Account-Id": e.codexID, "Accept": "application/json", "OpenAI-Beta": "codex-1", "Originator": "Codex Desktop"}})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.StatusCode == 429 {
			at := c.now().Add(time.Duration(c.cfg.Policy.UnknownProbeSeconds) * time.Second)
			if he.RetryAt != nil {
				at = *he.RetryAt
			}
			c.mu.Lock()
			c.creditRetry[e.account.ID] = at
			c.mu.Unlock()
		}
		return []domain.QuotaMetric{{Key: "reset-credits-unavailable", Label: "主动重置次数", Value: "尚未取得"}}
	}
	v, err := decodeJSON(b)
	if err != nil {
		return nil
	}
	root := object(v)
	var metrics []domain.QuotaMetric
	for _, entry := range [][3]string{{"available_count", "availableCount", "可用主动重置次数"}, {"applicable_available_count", "applicableAvailableCount", "当前适用重置次数"}} {
		if n, ok := number(first(root, entry[0], entry[1])); ok && n >= 0 {
			metrics = append(metrics, domain.QuotaMetric{Key: "reset-credits:" + entry[0], Label: entry[2], Value: scalarText(n), Unit: "次"})
		}
	}
	if credits, ok := root["credits"].([]any); ok {
		index := 0
		for _, item := range credits {
			m := object(item)
			if text(first(m, "reset_type", "resetType")) != "codex_rate_limits" || text(m["status"]) != "available" {
				continue
			}
			if end := text(first(m, "expires_at", "expiresAt")); end != "" {
				index++
				metrics = append(metrics, domain.QuotaMetric{Key: "reset-credit-expiry:" + itoa(index), Label: "重置资格到期 " + itoa(index), Value: end})
			}
		}
	}
	return metrics
}
