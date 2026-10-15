package adapters

import (
	"context"
	"net/http"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestLegacyRawIDCollidesWithNamespacedIDAndOnlyParticipantsAreQuarantined(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC)
	a := &quotaFixture{accounts: []domain.Account{fixtureAccount("legacy-normal", at)}}
	b := &quotaFixture{accounts: []domain.Account{fixtureAccount("x", at)}}
	c := &quotaFixture{accounts: []domain.Account{fixtureAccount("x", at)}}
	q, err := NewAggregator([]SourceInstance{
		{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: a},
		{ID: "source-b", Type: "readonly-http", Quota: b},
		{ID: "source-c", Type: "readonly-http", Quota: c},
	}, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	first, err := q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatal("normal namespaces changed legacy identity")
	}
	original := at
	at = at.Add(time.Second)
	// Preserve the documented legacy raw ID exactly; never silently rewrite it.
	a.accounts = []domain.Account{fixtureAccount("source-b::x", at)}
	accounts, err := q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, account := range accounts {
		if seen[account.ID] || account.ID == "source-b::x" {
			t.Fatal("ambiguous primary ID exposed or overwritten")
		}
		seen[account.ID] = true
		if account.SourceID == domain.DefaultSourceID {
			if account.ID != "legacy-normal" || account.State != "unknown" || account.QuotaError == "" || !account.CheckedAt.Equal(original) {
				t.Fatalf("collision did not restore unknown last-known evidence: %+v", account)
			}
		}
		if account.SourceID == "source-c" && (!domain.SourceConnected(account, q.Capabilities()) || account.State != "allowed") {
			t.Fatal("unrelated source was globally blocked")
		}
	}
	for _, health := range q.Capabilities().Sources {
		if health.ID == "source-c" {
			if !health.Connected || health.State != "healthy" {
				t.Fatal("unrelated source lost health")
			}
			continue
		}
		if health.Connected || health.State != "error" || health.Error == "" || health.LastSuccessAt == nil || !health.LastSuccessAt.Equal(original) {
			t.Fatalf("namespace participants not isolated: %+v", health)
		}
	}
}

func TestFirstPollCollisionStaysBlockedWhenOtherSourceIsNotDue(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC)
	a := &quotaFixture{accounts: []domain.Account{fixtureAccount("source-b::x", at)}}
	b := &quotaFixture{accounts: []domain.Account{fixtureAccount("x", at)}}
	q, err := NewAggregator([]SourceInstance{
		{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: a, PollInterval: 30 * time.Second},
		{ID: "source-b", Type: "readonly-http", Quota: b, PollInterval: 300 * time.Second},
	}, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	for _, advance := range []time.Duration{0, 30 * time.Second, 30 * time.Second} {
		at = at.Add(advance)
		accounts, err := q.Refresh(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(accounts) != 0 || q.Capabilities().ProxyConnected {
			t.Fatal("collision forgotten when a peer skipped polling")
		}
		for _, health := range q.Capabilities().Sources {
			if health.State != "error" || health.LastSuccessAt != nil {
				t.Fatal("rejected namespace gained success evidence")
			}
		}
	}
}

func TestReadonlyHTTPPerSourceDueAndManualInvalidationDoNotFreshenCachedEvidence(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC)
	fast, slow := httpFixture(t), httpFixture(t)
	fast.cfg.PollSeconds = 30
	slow.cfg.PollSeconds = 300
	fast.now = func() time.Time { return at }
	slow.now = fast.now
	fastCalls, slowCalls := 0, 0
	fast.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		fastCalls++
		return response(r, 200, normalizedDocument), nil
	})
	slow.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		slowCalls++
		return response(r, 200, normalizedDocument), nil
	})
	q, err := NewAggregator([]SourceInstance{
		{ID: "fast", Type: "readonly-http", Quota: fast, PollInterval: 30 * time.Second},
		{ID: "slow", Type: "readonly-http", Quota: slow, PollInterval: 300 * time.Second},
	}, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	first, err := q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	originalAccount := first[1]
	originalHealth := q.Capabilities().Sources[1]
	if q.NextRefreshAt() != at.Add(30*time.Second) {
		t.Fatal("per-source timer due time missing")
	}
	for i := 0; i < 2; i++ {
		at = at.Add(30 * time.Second)
		if _, err := q.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if fastCalls != 3 || slowCalls != 1 {
		t.Fatalf("slow source was fetched at fast cadence: %d/%d", fastCalls, slowCalls)
	}
	cached, err := q.Refresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cached[1].CheckedAt.Equal(originalAccount.CheckedAt) || !cached[1].Groups[0].CheckedAt.Equal(originalAccount.Groups[0].CheckedAt) || !cached[1].NextCheckAt.Equal(originalAccount.NextCheckAt) || !q.Capabilities().Sources[1].CheckedAt.Equal(originalHealth.CheckedAt) {
		t.Fatal("cached accounts or source health gained fresh timestamps")
	}
	at = at.Add(5 * time.Second)
	if err := q.InvalidateQuotaCache(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if fastCalls != 4 || slowCalls != 2 {
		t.Fatal("manual invalidation did not bypass each source due time")
	}
	// Even an actual request must retain the observation time supplied by the source.
	if cached, err := q.Refresh(ctx); err != nil || !cached[1].CheckedAt.Equal(originalAccount.CheckedAt) {
		t.Fatal("manual force invented a new quota observation")
	}
}

func TestEmptySourceStillHasDueTimeAndCPARetainsEarlierProbeHints(t *testing.T) {
	at := time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC)
	empty := &quotaFixture{}
	q, err := NewAggregator([]SourceInstance{{ID: "empty", Type: "readonly-http", Quota: empty, PollInterval: 30 * time.Second}}, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !q.NextRefreshAt().Equal(at.Add(30 * time.Second)) {
		t.Fatal("empty sources have no production wake-up")
	}
	account := fixtureAccount("a", at)
	account.NextCheckAt = at.Add(40 * time.Second)
	cpa := &quotaFixture{accounts: []domain.Account{account}}
	q, err = NewAggregator([]SourceInstance{{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: cpa, PollInterval: 300 * time.Second}}, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !q.NextRefreshAt().Equal(account.NextCheckAt) {
		t.Fatal("CPA earlier reset/freshness probe was delayed by configured cadence")
	}
}
