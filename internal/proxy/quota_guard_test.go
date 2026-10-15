package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

const healthyCredential = `{"files":[{"id":"codex-two.json","name":"codex-two.json","auth_index":"a2","provider":"codex","email":"two@example.org","disabled":false,"unavailable":false,"status":"active","id_token":{"chatgpt_account_id":"private-codex-two","plan_type":"plus"}}]}`

func throttledUpstream() *http.Response {
	b, _ := json.Marshal(map[string]any{"status_code": 429, "header": map[string]any{}, "body": "{}"})
	return response(200, string(b))
}

// guardClient answers the credential list with healthyCredential and every
// usage query with answer(). It counts usage queries.
func guardClient(t *testing.T, now *time.Time, answer func() *http.Response) (*Client, *int) {
	calls := 0
	c := testClient(t, testConfig(), func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			return response(200, healthyCredential), nil
		}
		b, _ := io.ReadAll(r.Body)
		v, _ := decodeJSON(b)
		if text(object(v)["url"]) != codexUsageURL {
			return response(404, `{}`), nil
		}
		calls++
		return answer(), nil
	})
	c.now = func() time.Time { return *now }
	return c, &calls
}

func TestThrottledUsageQueryIsNotExhaustion(t *testing.T) {
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	c, _ := guardClient(t, &now, throttledUpstream)
	a, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a[0].State == "limited" || a[0].State == "allowed" || a[0].QuotaError == "" {
		t.Fatalf("a throttled usage query became quota evidence: %+v", a[0])
	}
	if a[0].NextCheckAt.Before(now.Add(upstreamThrottleBackoff - time.Second)) {
		t.Fatalf("throttled usage endpoint re-queried at %v", a[0].NextCheckAt)
	}
}

func TestThrottledQueryKeepsObservedExhaustion(t *testing.T) {
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	throttled := false
	exhausted := `{"plan_type":"plus","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":7200},"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":400000}}}`
	c, calls := guardClient(t, &now, func() *http.Response {
		if throttled {
			return throttledUpstream()
		}
		return upstreamResponse(exhausted)
	})
	first, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first[0].State != "limited" || first[0].AvailableAt == nil {
		t.Fatalf("exhaustion not observed: %+v", first[0])
	}
	throttled = true
	now = now.Add(20 * time.Minute)
	if err := c.InvalidateQuotaCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Fatalf("usage queries %d, wanted 2", *calls)
	}
	if next[0].State != "limited" || next[0].AvailableAt == nil || !next[0].AvailableAt.Equal(*first[0].AvailableAt) {
		t.Fatalf("a throttled query discarded the observed exhaustion: %+v", next[0])
	}
}

func TestManualRefreshReusesAFreshAnswer(t *testing.T) {
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	c, calls := guardClient(t, &now, func() *http.Response { return upstreamResponse(whamAvailable) })
	refresh := func() {
		t.Helper()
		if err := c.InvalidateQuotaCache(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(5 * time.Second)
		refresh()
	}
	if *calls != 1 {
		t.Fatalf("rapid manual refreshes made %d usage queries, wanted 1", *calls)
	}
	now = now.Add(manualRefreshFloor)
	refresh()
	if *calls != 2 {
		t.Fatalf("manual refresh past the floor made %d usage queries, wanted 2", *calls)
	}
}

func TestDistantResetIsStillWatchedForEarlyResets(t *testing.T) {
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	c := New(testConfig())
	for _, tc := range []struct {
		until, want time.Duration
	}{
		{72 * time.Hour, 15 * time.Minute},
		{3 * time.Hour, 5 * time.Minute},
		{30 * time.Minute, 2 * time.Minute},
		{2 * time.Minute, 30 * time.Second},
	} {
		if got := c.resetProbe(now.Add(tc.until), now).Sub(now); got != tc.want {
			t.Fatalf("reset in %v probed after %v, wanted %v", tc.until, got, tc.want)
		}
	}
}

func TestClaudeFamilyWeeklyLimitIsModelScoped(t *testing.T) {
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	q, err := parseClaude([]byte(`{"five_hour":{"utilization":12,"resets_at":"2026-10-14T10:00:00Z"},"seven_day":{"utilization":40,"resets_at":"2026-10-18T00:00:00Z"},"seven_day_opus":{"utilization":100,"resets_at":"2026-10-17T00:00:00Z"}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if q.blocked || !q.allowed || len(q.windows) != 2 {
		t.Fatalf("Opus weekly limit stopped the whole account: %+v", q)
	}
	var opus bool
	for _, g := range q.groups {
		if g.ID == "seven_day_opus" {
			opus = g.Scope == "models" && len(g.Models) == 1 && g.Models[0] == "opus" && g.State == "limited"
		}
	}
	if !opus {
		t.Fatalf("Opus weekly group lost or unscoped: %+v", q.groups)
	}
}

func TestCodeReviewLimitIsShownButDoesNotStopConversations(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	c := New(testConfig())
	q, err := parseQuotaResponse([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0}},"code_review_rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_after_seconds":8000}}}`), "", now, true)
	if err != nil {
		t.Fatal(err)
	}
	a := c.finish(credential{account: domain.Account{ID: "one"}}, q, now.Add(time.Minute), now)
	if a.State != "allowed" {
		t.Fatalf("code review quota stopped conversations: %+v", a)
	}
	var review bool
	for _, g := range a.Groups {
		if g.Scope == "operation" {
			review = g.State == "limited" && g.AvailableAt != nil && g.AvailableAt.Equal(now.Add(8000*time.Second))
		}
	}
	if !review {
		t.Fatalf("code review limit discarded: %+v", a.Groups)
	}
}
