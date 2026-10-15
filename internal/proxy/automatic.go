package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"quota-session-orchestrator/internal/config"
)

// Unified fetch is a separate v0 capability, even when credentials use v8.
// It reads quota; it never calls reset, token import, chat probes or routing mutation.
func (c *Client) automaticQuota(ctx context.Context, key, prefix string, e credential) evidence {
	if e.account.AuthIndex == "" {
		return evidence{reason: "凭证缺少 auth_index，无法查询额度"}
	}
	provider := e.quotaProvider
	if provider == "" {
		provider = e.account.Provider
	}
	b, _, err := c.request(ctx, key, http.MethodPost, "/v0/management/quota/fetch", map[string]string{"auth_index": e.account.AuthIndex, "provider": provider})
	if err == nil {
		q, parseErr := parsePlugin(b, c.now().UTC())
		if parseErr != nil {
			return failedEvidence(parseErr, c.now())
		}
		return q
	}
	var he *HTTPError
	if !errors.As(err, &he) || (he.StatusCode != 404 && he.StatusCode != 405 && he.StatusCode != 501) {
		return failedEvidence(err, c.now())
	}
	switch e.account.Provider {
	case "claude":
		return c.claudeQuota(ctx, key, prefix, e)
	case "antigravity":
		return c.antigravityQuota(ctx, key, prefix, e)
	case "xai", "grok":
		return c.xaiQuota(ctx, key, prefix, e)
	default:
		return evidence{reason: "代理未提供此供应商的额度查询能力；不推定可用", source: "unsupported"}
	}
}

func (c *Client) loadModels(ctx context.Context, key, prefix string, e credential) []string {
	path := "/credentials/models"
	if prefix == "/v0/management" {
		path = "/auth-files/models"
	}
	b, _, err := c.request(ctx, key, http.MethodGet, prefix+path+"?name="+url.QueryEscape(e.account.ID), nil)
	if err != nil {
		return nil
	}
	v, err := decodeJSON(b)
	if err != nil {
		return nil
	}
	return readModels(object(v)["models"])
}

func (c *Client) claudeQuota(ctx context.Context, key, prefix string, e credential) evidence {
	headers := map[string]string{"User-Agent": "claude-cli/2.1.280 (external, cli)", "Content-Type": "application/json", "anthropic-beta": "oauth-2025-04-20"}
	b, err := c.apiCall(ctx, key, prefix, e, config.QuotaQuery{URL: "https://api.anthropic.com/api/oauth/usage", Headers: headers})
	if err != nil {
		return failedEvidence(err, c.now())
	}
	q, err := parseClaude(b, c.now().UTC())
	if err != nil {
		return failedEvidence(err, c.now())
	}
	// Profile adds presentation-only subscription information; its failure cannot erase quota.
	b, err = c.apiCall(ctx, key, prefix, e, config.QuotaQuery{URL: "https://api.anthropic.com/api/oauth/profile", Headers: headers})
	if err == nil {
		v, err := decodeJSON(b)
		if err == nil {
			root := object(v)
			account := object(root["account"])
			org := object(root["organization"])
			q.plan = text(org["organization_type"])
			if yes, _ := boolean(account["has_claude_max"]); yes {
				q.plan = "Max"
			} else if yes, _ := boolean(account["has_claude_pro"]); yes {
				q.plan = "Pro"
			}
		}
	}
	return q
}

func (c *Client) antigravityQuota(ctx context.Context, key, prefix string, e credential) evidence {
	if e.projectID == "" {
		return evidence{reason: "代理凭证清单未提供 project_id；未下载私有凭证文件", source: "antigravity"}
	}
	body, _ := json.Marshal(map[string]string{"project": e.projectID})
	endpoints := []string{"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"}
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"}
	var last error
	for _, endpoint := range endpoints {
		b, err := c.apiCallMethod(ctx, key, prefix, e, config.QuotaQuery{URL: endpoint, Headers: headers}, "POST", string(body))
		if err == nil {
			q, err := parsePlugin(b, c.now().UTC())
			if err == nil && len(q.groups) > 0 {
				q.source = "antigravity"
				return q
			}
			last = err
		} else {
			last = err
		}
		if ctx.Err() != nil {
			break
		}
		var he *HTTPError
		if errors.As(last, &he) && (he.StatusCode == 401 || he.StatusCode == 403 || he.StatusCode == 429) {
			break
		}
	}
	if last == nil {
		return evidence{reason: "Antigravity 未返回额度分组", source: "antigravity"}
	}
	return failedEvidence(last, c.now())
}

func (c *Client) xaiQuota(ctx context.Context, key, prefix string, e credential) evidence {
	headers := map[string]string{"x-xai-token-auth": "xai-grok-cli", "x-grok-client-version": "1.0.46", "accept": "*/*", "user-agent": "grok-pager/1.0.46 grok-shell/1.0.46 (macos; aarch64)"}
	if e.xaiUserID != "" {
		headers["x-userid"] = e.xaiUserID
	}
	result := evidence{source: "xai", checked: c.now().UTC()}
	successes := 0
	for _, endpoint := range []string{"https://cli-chat-proxy.grok.com/v1/billing?format=credits", "https://cli-chat-proxy.grok.com/v1/billing"} {
		b, err := c.apiCall(ctx, key, prefix, e, config.QuotaQuery{URL: endpoint, Headers: headers})
		if err != nil {
			result = combine(result, failedEvidence(err, c.now()))
			continue
		}
		q, err := parseXAI(b, c.now().UTC())
		if err != nil {
			result = combine(result, failedEvidence(err, c.now()))
			continue
		}
		result = combine(result, q)
		successes++
	}
	// A partial response is visible but not a complete sending authorization.
	result.allowed = successes == 2 && !result.failed && !result.blocked && completeWindows(result.windows)
	return result
}

func namedDuration(key string) int64 {
	switch key {
	case "five_hour":
		return 18000
	case "seven_day":
		return 604800
	}
	return 0
}

func groupChecked(now time.Time) time.Time { return now.UTC() }
