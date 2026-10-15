package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestPassiveSignalsAreNegativeOnlyAndAnchorSourceTime(t *testing.T) {
	now := time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)
	c := New(testConfig())
	entries, err := parseCredentials([]byte(`{"observed_at":"2026-10-11T03:00:00Z","files":[{"id":"one","provider":"codex","updated_at":"2026-10-11T02:00:00Z","quota":{"observed_at":"2026-10-11T00:00:00Z","signals":{"X-Codex-Primary-Used-Percent":"100","X-Codex-Primary-Window-Minutes":"300","X-Codex-Primary-Reset-After-Seconds":"3600","X-Codex-Secondary-Used-Percent":"100","X-Codex-Secondary-Reset-After-Seconds":"7200","X-Codex-Credits-Has-Credits":"False","X-Codex-Credits-Balance":"0"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	q := c.credentialEvidence(e, now)
	if !q.blocked || q.allowed || availability(q.windows) == nil || !availability(q.windows).Equal(time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("passive resets used list/credential time: %+v", q)
	}
	for _, values := range []map[string]any{
		{"X-Codex-Allowed": "True", "X-Codex-Primary-Used-Percent": "0"},
		{"X-Codex-Credits-Has-Credits": "False", "X-Codex-Credits-Balance": "0"},
		{"Anthropic-Ratelimit-Unified-5h-Status": "allowed", "Anthropic-Ratelimit-Unified-5h-Utilization": "0.53"},
	} {
		q := parsePassiveSignals(values, now)
		if q.allowed || q.blocked {
			t.Fatalf("passive/credit signal became universal availability/block: %+v", q)
		}
	}
}

func TestPassiveAbsoluteResetAndUnknownResetBlock(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	q := parsePassiveSignals(map[string]any{"X-Codex-Primary-Used-Percent": "100", "X-Codex-Primary-Reset-At": "2026-10-11T10:00:00+08:00", "X-Codex-Primary-Reset-After-Seconds": "10"}, now)
	if availability(q.windows) == nil || !availability(q.windows).Equal(now.Add(2*time.Hour)) {
		t.Fatal("absolute signal reset did not take precedence")
	}
	q = parsePassiveSignals(map[string]any{"X-Codex-Allowed": "False"}, now)
	if !q.blocked || availability(q.windows) != nil {
		t.Fatal("block without reset was lost")
	}
	q = parsePassiveSignals(map[string]any{"Anthropic-Ratelimit-Unified-5h-Status": "rejected", "Anthropic-Ratelimit-Unified-5h-Reset": "1791763200"}, now)
	if !q.blocked || !hasBlocked(q.windows) {
		t.Fatal("Anthropic rejection signal discarded")
	}
}

func TestReturnedWindowsDoNotMutateCache(t *testing.T) {
	c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return response(200, credentialFixture), nil
		}
		return upstreamResponse(whamAvailable), nil
	})
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	*a[0].Windows[0].UsedPercent = 99
	*a[0].Windows[0].ResetAt = time.Time{}
	a, err = c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *a[0].Windows[0].UsedPercent != 30 || a[0].Windows[0].ResetAt.IsZero() {
		t.Fatal("consumer mutated cached quota through returned pointers")
	}
}

func TestTopNextRetryAfterIsOptionalNegativeEvidence(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	entries, err := parseCredentials([]byte(`{"files":[{"id":"one","provider":"generic","unavailable":true,"next_retry_after":"2026-10-11T10:00:00+08:00"},{"id":"two","provider":"generic","unavailable":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := New(testConfig())
	for i, e := range entries {
		e.negative = c.credentialEvidence(e, now)
		a := c.finish(e, evidence{}, now.Add(time.Hour), now)
		if a.State != "limited" {
			t.Fatalf("unavailable lost: %+v", a)
		}
		if i == 0 && (a.AvailableAt == nil || !a.AvailableAt.Equal(now.Add(2*time.Hour))) {
			t.Fatalf("next retry ignored: %+v", a)
		}
		if i == 1 && a.AvailableAt != nil {
			t.Fatal("missing reset was invented")
		}
	}
}

func TestAdditionalLimitLabelsNeverInventModels(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	q, err := parseQuotaResponse([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":1}},"additional_rate_limits":[{"limit_name":"quota-bucket-not-a-model","rate_limit":{"allowed":true,"primary_window":{"used_percent":2}}}]}`), "", now, true)
	if err != nil {
		t.Fatal(err)
	}
	c := New(testConfig())
	a := c.finish(credential{account: domain.Account{ID: "one"}}, q, now.Add(time.Minute), now)
	if len(a.Models) != 0 || len(a.Windows) != 2 {
		t.Fatalf("additional limits/model mapping: %+v", a)
	}
}
