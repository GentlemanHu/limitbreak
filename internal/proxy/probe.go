package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

func (c *Client) probe(ctx context.Context, key, prefix string, e credential) evidence {
	var queries []config.QuotaQuery
	for _, q := range c.cfg.Queries {
		if strings.EqualFold(q.Provider, e.account.Provider) {
			queries = append(queries, q)
		}
	}
	if len(queries) == 0 && e.account.Provider == "codex" {
		queries = []config.QuotaQuery{{Provider: "codex", URL: codexUsageURL}}
	}
	if len(queries) == 0 {
		return c.automaticQuota(ctx, key, prefix, e)
	}
	if e.account.AuthIndex == "" {
		return evidence{reason: "credential has no auth_index for quota query"}
	}
	var result evidence
	allAllowed := true
	var oldest time.Time
	for _, query := range queries {
		var q evidence
		if query.PluginID != "" {
			// Plugins are addressed only by an explicitly configured ID. Never
			// discover arbitrary plugins or call their import/refresh operations.
			if strings.ContainsAny(query.PluginID, "/?#\\") {
				q = failedEvidence(errors.New("invalid quota plugin ID"), c.now())
			} else {
				b, _, err := c.request(ctx, key, http.MethodGet, prefix+"/plugins/"+url.PathEscape(query.PluginID)+"/quota?auth_index="+url.QueryEscape(e.account.AuthIndex), nil)
				if err != nil {
					q = failedEvidence(err, c.now())
				} else {
					q, err = parsePlugin(b, c.now().UTC())
					if err != nil {
						q = failedEvidence(err, c.now())
					}
				}
			}
		} else if query.URL != "" {
			b, err := c.apiCall(ctx, key, prefix, e, query)
			if err != nil {
				q = failedEvidence(err, c.now())
			} else {
				q, err = parseQuotaResponse(b, query.RateLimitPath, c.now().UTC(), query.URL == codexUsageURL)
				if err != nil {
					q = failedEvidence(err, c.now())
				}
			}
		} else {
			q = evidence{reason: "quota query has no explicit URL or plugin ID"}
		}
		if query.URL == codexUsageURL && !q.failed {
			q.metrics = append(q.metrics, c.codexCreditMetrics(ctx, key, prefix, e)...)
		}
		for _, m := range query.Models {
			q.models = appendUnique(q.models, m)
		}
		allAllowed = allAllowed && q.allowed && !q.blocked && !q.failed && completeWindows(q.windows) && c.fresh(q.checked, c.now())
		if oldest.IsZero() || (!q.checked.IsZero() && q.checked.Before(oldest)) {
			oldest = q.checked
		}
		result = combine(result, q)
	}
	result.allowed = allAllowed
	// All configured queries must be fresh, not merely the newest successful
	// query. Cached results retain this evidence timestamp across refreshes.
	result.checked = oldest
	return result
}

func failedEvidence(err error, now time.Time) evidence {
	q := evidence{failed: true, checked: now.UTC(), reason: err.Error()}
	var he *HTTPError
	if errors.As(err, &he) {
		if he.RetryAt != nil {
			q.retryAt = *he.RetryAt
		}
		// A 429 from the usage endpoint throttles the query itself; it says
		// nothing about the account's model quota. The failure stays unknown
		// (never allowed), and Retry-After bounds the next query.
	}
	return q
}

func (c *Client) apiCall(ctx context.Context, key, prefix string, e credential, q config.QuotaQuery) ([]byte, error) {
	return c.apiCallMethod(ctx, key, prefix, e, q, "GET", "")
}

func (c *Client) apiCallMethod(ctx context.Context, key, prefix string, e credential, q config.QuotaQuery, method, data string) ([]byte, error) {
	u, err := url.Parse(q.URL)
	if err != nil || u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("quota URL must be explicit HTTPS")
	}
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json"}
	for k, v := range q.Headers {
		if strings.ContainsAny(k+v, "\r\n") {
			return nil, errors.New("invalid quota header")
		}
		for existing := range headers {
			if strings.EqualFold(existing, k) {
				delete(headers, existing)
			}
		}
		headers[k] = v
	}
	if q.URL == codexUsageURL {
		if e.codexID == "" {
			return nil, errors.New("Codex credential has no parsed private account ID")
		}
		headers["Chatgpt-Account-Id"] = e.codexID
	}
	// Both documented API generations accept auth_index and the singular
	// header map. $TOKEN$ substitution occurs inside CPA, never in this process.
	payload := map[string]any{"auth_index": e.account.AuthIndex, "method": method, "url": q.URL, "header": headers}
	if data != "" {
		payload["data"] = data
	}
	path := "/requests/api-call"
	if prefix == "/v0/management" {
		path = "/api-call"
	}
	b, _, err := c.request(ctx, key, http.MethodPost, prefix+path, payload)
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	m := object(v)
	status, ok := number(m["status_code"])
	if !ok || status != float64(int(status)) || status < 100 || status > 599 {
		return nil, errors.New("api-call response missing upstream status_code")
	}
	if status < 200 || status >= 300 {
		he := &HTTPError{StatusCode: int(status)}
		if status == 429 {
			h := object(m["header"])
			if h == nil {
				h = object(m["headers"])
			}
			for k, v := range h {
				if strings.EqualFold(k, "Retry-After") {
					value := text(v)
					if list, ok := v.([]any); ok && len(list) > 0 {
						value = text(list[0])
					}
					he.RetryAt = parseRetryAfter(value, c.now())
				}
			}
			if he.RetryAt == nil {
				// No Retry-After: still back off this account's usage endpoint.
				t := c.now().Add(upstreamThrottleBackoff).UTC()
				he.RetryAt = &t
			}
		}
		return nil, he
	}
	body, ok := m["body"].(string)
	if !ok {
		return nil, errors.New("api-call response missing string body")
	}
	if len(body) > maxBody {
		return nil, errors.New("upstream quota response body too large")
	}
	return []byte(body), nil
}

func parseQuotaResponse(b []byte, path string, now time.Time, codex bool) (evidence, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return evidence{}, err
	}
	root := object(v)
	if root == nil {
		return evidence{}, errors.New("quota response is not an object")
	}
	checked := quotaTime(root, now)
	if path != "" {
		selected, ok := selectPath(v, path)
		if !ok {
			return evidence{}, errors.New("configured quota rate_limit_path is absent")
		}
		m := object(selected)
		if m == nil {
			return evidence{}, errors.New("configured quota rate_limit_path is not an object")
		}
		q := parseRateLimit(m, checked, "quota")
		q.plan = text(root["plan_type"])
		return q, nil
	}
	m := object(root["rate_limit"])
	if m == nil && !codex {
		m = root
	}
	if m == nil {
		return evidence{}, errors.New("quota response missing rate_limit")
	}
	q := parseRateLimit(m, checked, "quota")
	q.plan = text(root["plan_type"])
	q.models = readModels(root["models"])
	q.source = "api-call"
	if credits := object(root["credits"]); credits != nil {
		for _, field := range []string{"balance", "unlimited"} {
			if value, exists := credits[field]; exists {
				q.metrics = append(q.metrics, domain.QuotaMetric{Key: "credits:" + field, Label: map[string]string{"balance": "积分余额", "unlimited": "无限积分"}[field], Value: scalarText(value)})
			}
		}
	}
	q.groups = []domain.QuotaGroup{{ID: "account", Name: "共享额度", Scope: "account", Windows: cloneWindows(q.windows), CheckedAt: checked}}
	// The real WHAM schema can include additional named limits. Preserve every
	// blocked limit; do not interpret a limit name as a provider model ID.
	if additional, ok := root["additional_rate_limits"].([]any); ok {
		for _, v := range additional {
			item := object(v)
			r := object(item["rate_limit"])
			if r == nil {
				continue
			}
			label := text(item["limit_name"])
			if label == "" {
				label = "additional quota"
			}
			x := parseRateLimit(r, checked, label)
			x.groups = []domain.QuotaGroup{{ID: label, Name: label, Scope: "unknown", Windows: cloneWindows(x.windows), CheckedAt: checked}}
			allowed := q.allowed && x.allowed && completeWindows(x.windows)
			q = combine(q, x)
			q.allowed = allowed
		}
	}
	if r := object(root["code_review_rate_limit"]); r != nil {
		x := parseRateLimit(r, checked, "code review quota")
		// Code review has its own budget: it is shown and probed, but an
		// exhausted review quota does not stop a conversation from continuing.
		g := domain.QuotaGroup{ID: "code-review", Name: "代码审查额度", Scope: "operation", Windows: cloneWindows(x.windows), CheckedAt: checked}
		if x.blocked && !hasBlocked(g.Windows) {
			g.Windows = append(g.Windows, domain.Window{Name: "code review quota", Blocked: true})
		}
		q.groups = append(q.groups, g)
	}
	return q, nil
}

func quotaTime(root map[string]any, now time.Time) time.Time {
	for _, key := range []string{"updated_at", "checked_at", "time", "timestamp"} {
		if v, ok := root[key]; ok && v != nil {
			return parseTime(v)
		}
	}
	return now
}

// RateLimitPath supports dotted object keys and RFC6901 JSON pointers.
func selectPath(v any, path string) (any, bool) {
	var parts []string
	if strings.HasPrefix(path, "/") {
		parts = strings.Split(path[1:], "/")
		for i := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(parts[i], "~1", "/"), "~0", "~")
		}
	} else {
		parts = strings.Split(path, ".")
	}
	for _, part := range parts {
		m := object(v)
		if m == nil {
			return nil, false
		}
		var ok bool
		v, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return v, true
}
