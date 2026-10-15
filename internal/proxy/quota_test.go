package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

func TestQuotaExplicitAllowanceAndAllBlockedWindows(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body, state string
		available         bool
		offset            time.Duration
	}{
		{"missing allowed", `{"rate_limit":{"primary_window":{"used_percent":12},"secondary_window":{"used_percent":0}}}`, "unknown", false, 0},
		{"explicit allowance", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":12},"secondary_window":{"used_percent":0}}}`, "allowed", false, 0},
		{"allowed but incomplete window", `{"rate_limit":{"allowed":true,"primary_window":{}}}`, "unknown", false, 0},
		{"allowed but no windows", `{"rate_limit":{"allowed":true}}`, "unknown", false, 0},
		{"blocked without reset", `{"rate_limit":{"allowed":false,"limit_reached":true}}`, "limited", false, 0},
		{"full window without allowed", `{"rate_limit":{"primary_window":{"used_percent":100}}}`, "limited", false, 0},
		{"all blocked max", `{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_after_seconds":60},"secondary_window":{"used_percent":100,"reset_after_seconds":7200}}}`, "limited", true, 2 * time.Hour},
		{"one blocked reset unknown", `{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_after_seconds":60},"secondary_window":{"used_percent":100}}}`, "limited", false, 0},
		{"old reset never automatically allowed", `{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":"2026-10-10T00:00:00Z"}}}`, "limited", true, -24 * time.Hour},
		{"absolute preferred timezone", `{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":"2026-10-11T10:00:00+08:00","reset_after_seconds":10}}}`, "limited", true, 2 * time.Hour},
		{"stale allowance", `{"updated_at":"2026-10-10T23:00:00Z","rate_limit":{"allowed":true,"primary_window":{"used_percent":0}}}`, "unknown", false, 0},
		{"invalid timestamp", `{"updated_at":"invalid","rate_limit":{"allowed":true,"primary_window":{"used_percent":0}}}`, "unknown", false, 0},
		{"additional limit cannot be discarded", `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0}},"additional_rate_limits":[{"limit_name":"real-limit-name","rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_after_seconds":8000}}}]}`, "limited", true, 8000 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(testConfig())
			q, err := parseQuotaResponse([]byte(tc.body), "", now, true)
			if err != nil {
				t.Fatal(err)
			}
			a := c.finish(credential{account: domain.Account{ID: "one"}}, q, now.Add(time.Minute), now)
			if a.State != tc.state {
				t.Fatalf("state=%s wanted %s evidence=%+v", a.State, tc.state, q)
			}
			if (a.AvailableAt != nil) != tc.available {
				t.Fatalf("availability=%v wanted present=%v", a.AvailableAt, tc.available)
			}
			if tc.available && !a.AvailableAt.Equal(now.Add(tc.offset)) {
				t.Fatalf("availability=%v wanted %v", a.AvailableAt, now.Add(tc.offset))
			}
			if len(a.Models) != 0 {
				t.Fatal("limit label became invented model mapping")
			}
		})
	}
}

func TestCredentialRelativeResetsDoNotSlide(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	cfg := testConfig()
	c := testClient(t, cfg, func(*http.Request) (*http.Response, error) {
		return response(200, `{"files":[{"id":"one","provider":"generic","unavailable":true,"status_message":"{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}"}]}`), nil
	})
	c.now = func() time.Time { return now }
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first := a[0].AvailableAt
	if first == nil || !first.Equal(now.Add(time.Hour)) {
		t.Fatalf("first anchor: %+v", a)
	}
	now = now.Add(30 * time.Minute)
	a, err = c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a[0].AvailableAt == nil || !a[0].AvailableAt.Equal(*first) {
		t.Fatalf("relative reset slid: %v vs %v", a[0].AvailableAt, first)
	}
	now = now.Add(2 * time.Hour)
	a, err = c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a[0].State != "limited" || !a[0].AvailableAt.Equal(*first) {
		t.Fatal("expired management reset automatically permitted request")
	}
}

func TestCredentialUpdatedAtAndAbsoluteReset(t *testing.T) {
	c := New(testConfig())
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		payload string
		want    time.Time
	}{
		{`{"files":[{"id":"relative","provider":"generic","updated_at":"2026-10-11T12:00:00+08:00","status_message":{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}}]}`, time.Date(2026, 10, 11, 5, 0, 0, 0, time.UTC)},
		{`{"files":[{"id":"absolute","provider":"generic","updated_at":"2026-10-11T12:00:00+08:00","status_message":{"error":{"type":"usage_limit_reached","resets_in_seconds":3600,"resets_at":"2026-10-12T02:00:00+08:00"}}}]}`, time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)},
	} {
		entries, err := parseCredentials([]byte(tc.payload))
		if err != nil {
			t.Fatal(err)
		}
		q := c.credentialEvidence(entries[0], now)
		available := availability(q.windows)
		if !q.blocked || available == nil || !available.Equal(tc.want) {
			t.Fatalf("anchor parsed incorrectly: %+v availability=%v", q, available)
		}
	}
}

func TestModelCooldownAndRecoveryRequiresFreshQuota(t *testing.T) {
	c := New(testConfig())
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	v, err := parseCredentials([]byte(`{"credentials":[{"id":"one","provider":"generic","models":["real-model"],"model_states":{"real-model":{"unavailable":true,"quota":{"exceeded":true,"next_retry_after":"2026-10-12T00:00:00Z"}}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := v[0]
	e.negative = c.credentialEvidence(e, now)
	a := c.finish(e, evidence{}, now.Add(time.Hour), now)
	if a.State != "limited" || len(a.Models) != 1 || !hasBlocked(a.Windows) {
		t.Fatalf("model block discarded: %+v", a)
	}
	q, err := parseQuotaResponse([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":10}}}`), "", now, true)
	if err != nil {
		t.Fatal(err)
	}
	a = c.finish(e, q, now.Add(time.Hour), now)
	if a.State != "limited" || !hasBlocked(a.Windows) || len(a.Models) != 1 {
		t.Fatalf("generic allowance erased independent model restriction: %+v", a)
	}
	a = c.finish(e, q, now.Add(time.Hour), now.Add(time.Hour))
	if a.State != "limited" {
		t.Fatal("stale positive quota bypassed model cooldown")
	}
}

func TestPluginActualCamelCaseSchema(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		body             string
		allowed, blocked bool
		available        bool
	}{
		{`{"subscription":{"plan":"plus","tierName":"Plus"},"summary":[{"key":"quota","label":"Remaining","value":0.5,"unit":"%"}],"serverTimeOffsetMs":0,"groups":[{"displayName":"Quota","buckets":[{"window":"daily","remainingFraction":0.5,"resetTime":"2026-10-12T00:00:00Z","description":"Daily"}]}]}`, true, false, false},
		{`{"groups":[{"displayName":"Quota","buckets":[{"window":"daily","resetTime":"2026-10-12T00:00:00Z"}]}]}`, false, false, false},
		{`{"summary":[{"key":"quota","label":"Remaining","value":100}]}`, false, false, false},
		{`{"groups":[{"buckets":[{"remainingFraction":0,"resetTime":"2026-10-12T00:00:00Z"},{"remainingFraction":0,"resetTime":"2026-10-13T00:00:00Z"}]}]}`, false, true, true},
		{`{"groups":[{"buckets":[{"remainingFraction":0}]}]}`, false, true, false},
		{`{"groups":[{"buckets":[{"remainingFraction":1.5}]}]}`, false, false, false},
	} {
		q, err := parsePlugin([]byte(tc.body), now)
		if err != nil {
			t.Fatal(err)
		}
		if q.allowed != tc.allowed || q.blocked != tc.blocked || (availability(q.windows) != nil) != tc.available {
			t.Fatalf("plugin parse=%+v", q)
		}
		if q.allowed && !completeWindows(q.windows) {
			t.Fatal("incomplete plugin allowance")
		}
		if tc.available && !availability(q.windows).Equal(now.Add(48*time.Hour)) {
			t.Fatal("plugin did not combine all blocked windows")
		}
	}
}

func TestDeclarativePathAllQueriesAndProbeDurations(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	cfg := testConfig()
	cfg.PollSeconds = 77
	cfg.Policy.UnknownProbeSeconds = 123
	cfg.Policy.LongProbeSeconds = 456
	cfg.Policy.SafetySeconds = 11
	c := New(cfg)
	body := []byte(`{"data":{"usage":{"allowed":true,"primary_window":{"used_percent":20}}}}`)
	for _, path := range []string{"data.usage", "/data/usage"} {
		q, err := parseQuotaResponse(body, path, now, false)
		if err != nil || !q.allowed {
			t.Fatalf("path=%s q=%+v err=%v", path, q, err)
		}
	}
	q, err := parseQuotaResponse(body, "absent", now, false)
	if err == nil || q.allowed {
		t.Fatal("absent declared path allowed")
	}
	allowed, err := parseQuotaResponse([]byte(whamAvailable), "", now, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.nextProbe(allowed, now, 0); !got.Equal(now.Add(77 * time.Second)) {
		t.Fatalf("poll duration: %v", got)
	}
	if got := c.nextProbe(evidence{}, now, 0); !got.Equal(now.Add(123 * time.Second)) {
		t.Fatalf("unknown duration: %v", got)
	}
	if got := c.nextProbe(evidence{failed: true}, now, 2); !got.Equal(now.Add(246 * time.Second)) {
		t.Fatalf("backoff duration: %v", got)
	}
	reset := now.Add(50 * time.Second)
	blocked := evidence{blocked: true, windows: []domain.Window{{Blocked: true, ResetAt: &reset}}}
	if got := c.nextProbe(blocked, now, 0); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("near-reset read-only probe duration: %v", got)
	}
	blocked.retryAt = now.Add(500 * time.Second)
	if got := c.nextProbe(blocked, now, 0); !got.Equal(now.Add(500 * time.Second)) {
		t.Fatalf("retry-after floor: %v", got)
	}
}

func TestMultipleQueriesMustAllExplicitlyAllow(t *testing.T) {
	cfg := testConfig()
	cfg.Queries = []config.QuotaQuery{{Provider: "other", URL: "https://quota.invalid/a", RateLimitPath: "rate_limit", Models: []string{"real-model"}}, {Provider: "other", URL: "https://quota.invalid/b", Models: []string{"real-model"}}}
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return response(200, `{"credentials":[{"id":"other","auth_index":"a1","provider":"other"}]}`), nil
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p["url"] == "https://quota.invalid/a" {
			return upstreamResponse(whamAvailable), nil
		}
		return upstreamResponse(`{"rate_limit":{"primary_window":{"used_percent":0}}}`), nil
	})
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a[0].State != "unknown" {
		t.Fatalf("one allowance wrongly covered another query: %+v", a)
	}
}
