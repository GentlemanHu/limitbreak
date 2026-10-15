package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// Only this internal representation retains the private Codex account ID.
// Token fields and the original credential response are never cached or exported.
type credential struct {
	account       domain.Account
	codexID       string
	updated       time.Time
	nextRetry     *time.Time
	status        string
	unavailable   bool
	statusDetail  any
	quota         any
	modelStates   map[string]any
	signature     string
	negative      evidence
	projectID     string
	xaiUserID     string
	quotaProvider string
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("invalid management JSON response")
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("multiple JSON values in management response")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid trailing management JSON")
	}
	return v, nil
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func text(v any) string           { s, _ := v.(string); return s }
func identifier(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	}
	return ""
}
func boolean(v any) (bool, bool) { b, ok := v.(bool); return b, ok }
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}
func first(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func parseCredentials(b []byte) ([]credential, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	var entries []any
	if a, ok := v.([]any); ok {
		entries = a
	} else {
		m := object(v)
		for _, k := range []string{"credentials", "files"} {
			if a, ok := m[k].([]any); ok {
				entries = a
				break
			}
		}
		if entries == nil {
			return nil, errors.New("management credential list schema is unsupported")
		}
	}
	out := make([]credential, 0, len(entries))
	for _, raw := range entries {
		m := object(raw)
		if m == nil {
			return nil, errors.New("invalid credential list entry")
		}
		e := credential{}
		e.account.ID = identifier(first(m, "id", "name"))
		e.account.AuthIndex = identifier(first(m, "auth_index", "authIndex"))
		if e.account.ID == "" {
			e.account.ID = e.account.AuthIndex
		}
		if e.account.ID == "" {
			return nil, errors.New("credential has no safe identity")
		}
		e.account.Provider = strings.ToLower(text(first(m, "provider", "type")))
		e.account.Email = text(m["email"])
		e.account.Plan = text(first(m, "plan_type", "plan"))
		e.quotaProvider = text(m["quota_provider"])
		e.account.Disabled, _ = boolean(m["disabled"])
		e.account.Models = readModels(m["models"])
		for _, fields := range []map[string]any{m, object(m["metadata"]), object(m["attributes"])} {
			if e.projectID == "" {
				e.projectID = text(first(fields, "project_id", "projectId", "gemini_virtual_project", "gemini_virtual_project_id"))
			}
			if e.xaiUserID == "" {
				e.xaiUserID = text(first(fields, "user_id", "userId", "userid", "x_userid"))
			}
		}
		e.status = strings.ToLower(text(m["status"]))
		e.unavailable, _ = boolean(m["unavailable"])
		e.statusDetail = m["status_message"]
		if s := text(e.statusDetail); s != "" {
			if parsed, err := decodeJSON([]byte(s)); err == nil {
				e.statusDetail = parsed
			}
		}
		e.quota = m["quota"]
		e.modelStates = object(first(m, "model_states", "modelStates", "model_quotas"))
		e.updated = parseTime(first(m, "updated_at", "updatedAt", "time"))
		if reset := parseTime(m["next_retry_after"]); !reset.IsZero() {
			e.nextRetry = &reset
		}
		for model := range e.modelStates {
			e.account.Models = appendUnique(e.account.Models, model)
		}
		// Management APIs expose a parsed, safe subset of the id_token claims.
		// Never decode a raw JWT or load/download the underlying auth file.
		claims := object(m["id_token"])
		if e.account.Provider == "codex" {
			e.codexID = text(first(claims, "chatgpt_account_id", "account_id"))
			if e.account.Plan == "" {
				e.account.Plan = text(first(claims, "plan_type", "chatgpt_plan_type"))
			}
			if e.account.Email == "" {
				e.account.Email = text(claims["email"])
			}
		}
		// Hash only status material; secrets and refreshed tokens do not affect
		// the first-observation anchor for an unchanged relative reset.
		safe, _ := json.Marshal([]any{e.account.AuthIndex, e.account.Provider, e.codexID, e.projectID, e.xaiUserID, e.quotaProvider, e.nextRetry, e.status, e.unavailable, e.statusDetail, e.quota, e.modelStates})
		h := sha256.Sum256(safe)
		e.signature = hex.EncodeToString(h[:])
		out = append(out, e)
	}
	return out, nil
}

func readModels(v any) []string {
	result := []string{}
	if a, ok := v.([]any); ok {
		for _, item := range a {
			model := text(item)
			if m := object(item); m != nil {
				model = text(first(m, "id", "name", "model"))
			}
			if model != "" {
				result = appendUnique(result, model)
			}
		}
	}
	return result
}

func appendUnique(a []string, s string) []string {
	for _, v := range a {
		if v == s {
			return a
		}
	}
	return append(a, s)
}

// parseTime preserves explicit ISO offsets and accepts Unix timestamps from
// the documented upstream reset_at field (seconds, not a guessed timezone).
func parseTime(v any) time.Time {
	if s := text(v); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	if n, ok := number(v); ok && n > 0 && n < 253402300800 {
		return time.Unix(int64(n), int64((n-float64(int64(n)))*1e9)).UTC()
	}
	return time.Time{}
}

func (c *Client) credentialEvidence(e credential, now time.Time) evidence {
	anchor := e.updated
	if anchor.IsZero() {
		byHash := c.anchors[e.account.ID]
		if byHash == nil {
			byHash = map[string]time.Time{}
			c.anchors[e.account.ID] = byHash
		}
		var ok bool
		anchor, ok = byHash[e.signature]
		if !ok {
			if len(byHash) >= 32 {
				var oldest string
				var t time.Time
				for k, v := range byHash {
					if t.IsZero() || v.Before(t) {
						oldest = k
						t = v
					}
				}
				delete(byHash, oldest)
			}
			anchor = now
			byHash[e.signature] = anchor
		}
	}
	q := evidence{}
	// Credential quota/status is negative evidence only; an expired cooldown
	// is never used as a positive signal that the provider allows requests.
	for _, v := range []any{e.quota, e.statusDetail} {
		q = combine(q, parseNegative(v, anchor, "credential"))
	}
	for model, state := range e.modelStates {
		negative := parseNegative(state, anchor, "model:"+model)
		for i := range negative.windows {
			if !strings.HasPrefix(negative.windows[i].Name, "model:"+model) {
				negative.windows[i].Name = "model:" + model + ":" + negative.windows[i].Name
			}
		}
		q = combine(q, negative)
	}
	if e.unavailable || e.status == "error" || e.status == "unavailable" || e.status == "cooldown" || (e.nextRetry != nil && e.nextRetry.After(now)) {
		q.blocked = true
		if q.checked.IsZero() {
			q.checked = anchor
		}
		q.reason = "credential is unavailable; live quota verification required"
		if !hasBlocked(q.windows) {
			q.windows = append(q.windows, domain.Window{Name: "credential status", Blocked: true, ResetAt: e.nextRetry})
		}
	}
	return q
}

func parseNegative(v any, anchor time.Time, name string) evidence {
	m := object(v)
	if m == nil {
		return evidence{}
	}
	if signals := object(m["signals"]); signals != nil {
		if observed := parseTime(m["observed_at"]); !observed.IsZero() {
			anchor = observed
		}
		return parsePassiveSignals(signals, anchor)
	}
	q := parseRateLimit(m, anchor, name)
	q.allowed = false // Management status cannot authorize resumption.
	if b, ok := boolean(m["exceeded"]); ok && b {
		q.blocked = true
	}
	if b, ok := boolean(m["unavailable"]); ok && b {
		q.blocked = true
	}
	if errObj := object(m["error"]); errObj != nil {
		errorType := text(first(errObj, "type", "code"))
		if errorType == "usage_limit_reached" || errorType == "rate_limit_exceeded" || errorType == "quota_exceeded" {
			q.blocked = true
			q.windows = append(q.windows, domain.Window{Name: name, Blocked: true, ResetAt: resetAt(errObj, anchor)})
		}
	}
	if nested := object(m["quota"]); nested != nil {
		q = combine(q, parseNegative(nested, anchor, name))
	}
	if r := object(m["rate_limit"]); r != nil {
		q = combine(q, parseNegative(r, anchor, name))
	}
	if q.blocked && !hasBlocked(q.windows) {
		q.windows = append(q.windows, domain.Window{Name: name, Blocked: true, ResetAt: resetAt(m, anchor)})
	}
	return q
}
