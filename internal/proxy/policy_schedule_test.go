package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

func TestPolicyEarlyProbeRefreshesBlockedQuotaWithoutInventingRecovery(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	origin := now
	reset := now.Add(10 * time.Minute)
	cfg := testConfig()
	cfg.PollSeconds = 3600
	cfg.Queries = []config.QuotaQuery{{Provider: "other", URL: "https://quota.invalid/usage", Models: []string{"opus"}}}
	var probes atomic.Int32
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return response(200, `{"credentials":[{"id":"a","provider":"other","auth_index":"a1"}]}`), nil
		}
		probes.Add(1)
		return upstreamResponse(fmt.Sprintf(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":%q}}}`, reset.Format(time.RFC3339))), nil
	})
	c.now = func() time.Time { return now }
	accounts, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].State != "limited" || !c.NextRefreshAt().Equal(origin.Add(120*time.Second)) {
		t.Fatalf("missing early read-only probe: %+v due=%v", accounts, c.NextRefreshAt())
	}
	p := cfg.Policy
	p.EarlyProbeSeconds = 60
	if err := c.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !c.NextRefreshAt().Equal(origin.Add(60*time.Second)) || !c.cache["a"].quota.checked.Equal(origin) || probes.Load() != 1 {
		t.Fatal("SetPolicy failed to shorten cadence or fabricated new evidence")
	}
	now = origin.Add(59 * time.Second)
	accounts, err = c.Refresh(context.Background())
	if err != nil || probes.Load() != 1 || !accounts[0].CheckedAt.Equal(origin) {
		t.Fatal("quota was probed or relabelled before the early deadline")
	}
	now = origin.Add(60 * time.Second)
	accounts, err = c.Refresh(context.Background())
	if err != nil || probes.Load() != 2 || accounts[0].State == "allowed" || !accounts[0].CheckedAt.Equal(now) {
		t.Fatal("early real probe failed or blocked quota was treated as recovered")
	}
	now = reset.Add(-time.Duration(p.ResetLookaheadSeconds) * time.Second)
	accounts, err = c.Refresh(context.Background())
	if err != nil || probes.Load() != 3 || accounts[0].State == "allowed" || !accounts[0].NextCheckAt.Equal(now.Add(30*time.Second)) {
		t.Fatal("entering the configured lookahead did not switch to near-reset cadence")
	}
	now = reset.Add(time.Second)
	accounts, err = c.Refresh(context.Background())
	if err != nil || accounts[0].State == "allowed" || accounts[0].AvailableAt == nil || !accounts[0].AvailableAt.Equal(reset) || !accounts[0].NextCheckAt.Equal(now.Add(30*time.Second)) {
		t.Fatal("expired reset cleared a blocked window or drifted its evidence")
	}
}

func TestResetLookaheadSelectsCadenceWithoutChangingEvidence(t *testing.T) {
	c := New(testConfig())
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := now.Add(150 * time.Second)
	q := evidence{blocked: true, checked: now, windows: []domain.Window{{Blocked: true, ResetAt: &reset}}}
	if got := c.nextProbe(q, now, 0); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatal("reset within default lookahead missed near-reset cadence")
	}
	p := c.cfg.Policy
	p.ResetLookaheadSeconds = 100
	if err := c.SetPolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := c.nextProbe(q, now, 0); !got.Equal(now.Add(120*time.Second)) || !q.checked.Equal(now) || !q.windows[0].ResetAt.Equal(reset) || q.allowed {
		t.Fatal("lookahead did not select early cadence or changed blocked evidence")
	}
}

func TestUnknownResetAndRetryAfterStillBoundEarlyProbes(t *testing.T) {
	c := New(testConfig())
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := now.Add(time.Minute)
	q := evidence{blocked: true, windows: []domain.Window{{Blocked: true, ResetAt: &reset}, {Blocked: true}}}
	if got := c.nextProbe(q, now, 0); !got.Equal(now.Add(time.Duration(c.cfg.Policy.LongProbeSeconds) * time.Second)) {
		t.Fatal("one known reset replaced an unknown shared-window reset")
	}
	q.windows = q.windows[:1]
	q.retryAt = now.Add(time.Hour)
	if got := c.nextProbe(q, now, 0); !got.Equal(q.retryAt) {
		t.Fatal("early probe bypassed upstream Retry-After")
	}
}

func TestClockChangedPreservesEvidenceAnchorsAndRearmsCadence(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	original := now
	reset := now.Add(time.Hour)
	cfg := testConfig()
	cfg.Queries = []config.QuotaQuery{{Provider: "other", URL: "https://quota.invalid/usage", Models: []string{"opus"}}}
	var probes atomic.Int32
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return response(200, `{"credentials":[{"id":"a","provider":"other","auth_index":"a1"}]}`), nil
		}
		probes.Add(1)
		return upstreamResponse(fmt.Sprintf(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100,"reset_at":%q}}}`, reset.Format(time.RFC3339))), nil
	})
	c.now = func() time.Time { return now }
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.anchors["a"] = map[string]time.Time{"fingerprint": original}
	c.creditRetry["a"] = reset
	now = now.Add(-time.Hour)
	if err := c.ClockChanged(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached := c.cache["a"]
	if !cached.quota.checked.Equal(original) || !cached.quota.windows[0].ResetAt.Equal(reset) || !c.anchors["a"]["fingerprint"].Equal(original) || !c.creditRetry["a"].Equal(reset) || !c.NextRefreshAt().Equal(now.Add(30*time.Second)) {
		t.Fatal("clock change rewrote observed time/reset/protection instead of cadence")
	}
	accounts, err := c.Refresh(context.Background())
	if err != nil || probes.Load() != 1 || !accounts[0].CheckedAt.Equal(original) || accounts[0].State == "allowed" {
		t.Fatal("clock change caused an immediate probe or authorized future evidence")
	}
	now = now.Add(30 * time.Second)
	accounts, err = c.Refresh(context.Background())
	if err != nil || probes.Load() != 2 || !accounts[0].CheckedAt.Equal(now) {
		t.Fatal("rebased deadline never requested a real quota observation")
	}
}

func TestClockChangedAndManualRefreshCannotBypassManagementGuards(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			original := now
			var requests atomic.Int32
			c := testClient(t, testConfig(), func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				r := response(status, "")
				r.Header.Set("Retry-After", "600")
				return r, nil
			})
			c.now = func() time.Time { return now }
			if _, err := c.Refresh(context.Background()); err == nil {
				t.Fatal("management refusal accepted")
			}
			now = now.Add(-time.Hour)
			if err := c.ClockChanged(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := c.InvalidateQuotaCache(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := c.SetPolicy(context.Background(), testConfig().Policy); err != nil {
				t.Fatal(err)
			}
			_, err := c.Refresh(context.Background())
			var refusal *HTTPError
			if !errors.As(err, &refusal) || refusal.StatusCode != status || requests.Load() != 1 {
				t.Fatal("rollback/manual refresh/policy bypassed a protected management request")
			}
			if status == 429 && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(original.Add(600*time.Second))) {
				t.Fatal("clock change rebased a hard management Retry-After timestamp")
			}
		})
	}
}

func TestClockChangedPreservesUpstreamRetryAfter(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	original := now
	cfg := testConfig()
	cfg.Queries = []config.QuotaQuery{{Provider: "other", URL: "https://quota.invalid/usage", Models: []string{"opus"}}}
	var probes atomic.Int32
	c := testClient(t, cfg, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return response(200, `{"credentials":[{"id":"a","provider":"other","auth_index":"a1"}]}`), nil
		}
		probes.Add(1)
		return response(200, `{"status_code":429,"header":{"Retry-After":["600"]},"body":""}`), nil
	})
	c.now = func() time.Time { return now }
	if _, err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-time.Hour)
	if err := c.ClockChanged(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.InvalidateQuotaCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	accounts, err := c.Refresh(context.Background())
	if err != nil || probes.Load() != 1 || accounts[0].State == "allowed" || !c.cache["a"].next.Equal(original.Add(600*time.Second)) || !c.cache["a"].quota.checked.Equal(original) {
		t.Fatal("clock change/manual refresh bypassed or relabelled upstream Retry-After")
	}
}

func TestQuotaScheduleReadersAndMutationsShareNoCacheRace(t *testing.T) {
	var requests atomic.Int32
	c := testClient(t, testConfig(), func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return response(200, `{"credentials":[]}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	operations := []func() error{
		func() error { _, err := c.Refresh(ctx); return err },
		func() error { return c.SetPolicy(ctx, testConfig().Policy) },
		func() error { return c.ClockChanged(ctx) },
		func() error { return c.InvalidateQuotaCache(ctx) },
		func() error { _ = c.NextRefreshAt(); _ = c.Capabilities(); return nil },
	}
	var workers sync.WaitGroup
	errors := make(chan error, len(operations))
	for _, operation := range operations {
		workers.Add(1)
		go func(operation func() error) {
			defer workers.Done()
			for i := 0; i < 20; i++ {
				if err := operation(); err != nil {
					errors <- err
					return
				}
			}
		}(operation)
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	if requests.Load() != 20 || c.NextRefreshAt().IsZero() {
		t.Fatal("empty source schedule disappeared or policy caused extra requests")
	}
}
