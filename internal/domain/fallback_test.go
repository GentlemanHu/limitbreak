package domain

import (
	"testing"
	"time"
)

func TestQuotaQueryThrottled(t *testing.T) {
	cases := map[string]bool{
		"":                                   false,
		"quota request returned HTTP 429":    true,
		"quota request returned HTTP 503":    true,
		"context deadline exceeded":          true,
		"dial tcp: i/o timeout":              true,
		"quota request returned HTTP 401":    false,
		"quota request returned HTTP 403":    false,
		"no explicit, fresh quota allowance": false,
		"quota source refresh failed; stale": false,
	}
	for reason, want := range cases {
		if got := QuotaQueryThrottled(reason); got != want {
			t.Errorf("QuotaQueryThrottled(%q) = %v, want %v", reason, got, want)
		}
	}
}

func TestResetPassed(t *testing.T) {
	failure := time.Date(2026, 10, 14, 15, 44, 0, 0, time.UTC)
	reset := failure.Add(65 * time.Minute)
	later := reset.Add(48 * time.Hour)
	full, partial := 100.0, 74.0
	account := func(ws ...Window) Account { return Account{Windows: ws} }
	five := Window{Name: "observed anthropic:5h", Blocked: true, UsedPercent: &full, ResetAt: &reset}
	week := Window{Name: "observed anthropic:7d", UsedPercent: &partial, ResetAt: &later}
	safety := 5 * time.Minute

	if at, ok := ResetPassed(account(five, week), "claude-opus-4-8", failure, safety, reset.Add(time.Minute)); at == nil || !at.Equal(reset) || ok {
		t.Fatalf("before reset+safety: at=%v ok=%v", at, ok)
	}
	if at, ok := ResetPassed(account(five, week), "claude-opus-4-8", failure, safety, reset.Add(safety)); at == nil || !ok {
		t.Fatalf("after reset+safety: at=%v ok=%v", at, ok)
	}
	// A failure newer than the reset is not explained by the block: no repeat.
	if at, _ := ResetPassed(account(five), "", reset.Add(time.Second), safety, reset.Add(time.Hour)); at != nil {
		t.Fatalf("failure after reset must not fall back, got %v", at)
	}
	// A block without a reset time keeps waiting for real evidence.
	if at, _ := ResetPassed(account(Window{Name: "x", Blocked: true}), "", failure, 0, later); at != nil {
		t.Fatalf("block without reset must not fall back, got %v", at)
	}
	// No recorded block at all is no evidence.
	if at, _ := ResetPassed(account(week), "", failure, 0, later); at != nil {
		t.Fatalf("no block must not fall back, got %v", at)
	}
	// The latest applicable reset wins; another model's window is ignored.
	other := later.Add(time.Hour)
	foreign := Window{Name: "model:claude-sonnet-4-5", Blocked: true, ResetAt: &other}
	if at, _ := ResetPassed(account(five, foreign), "claude-opus-4-8", failure, 0, later); at == nil || !at.Equal(reset) {
		t.Fatalf("foreign model window applied: %v", at)
	}
	if at, _ := ResetPassed(account(five, foreign), "claude-sonnet-4-5", failure, 0, later); at == nil || !at.Equal(other) {
		t.Fatalf("own model window ignored: %v", at)
	}
}
