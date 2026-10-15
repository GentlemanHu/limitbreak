package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

type invalidatingQuota struct {
	fixtureQuota
	providers []string
}

func (q *invalidatingQuota) InvalidateProvider(_ context.Context, provider string) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.providers = append(q.providers, provider)
	return 3, nil
}

func TestReceiveSignalInvalidatesProviderOnceAndNeverDispatches(t *testing.T) {
	e, _, b := fixture(t)
	q := &invalidatingQuota{}
	e.quota = q
	ctx := context.Background()
	sig := domain.ResetSignal{Source: "hook", Provider: "Codex", ID: "r1"}
	if n, err := e.ReceiveSignal(ctx, sig); err != nil || n != 3 {
		t.Fatalf("receive: %d %v", n, err)
	}
	if _, err := e.ReceiveSignal(ctx, sig); !errors.Is(err, domain.ErrDuplicateSignal) {
		t.Fatalf("duplicate: %v", err)
	}
	if len(q.providers) != 1 || q.providers[0] != "codex" || b.count() != 0 {
		t.Fatalf("invalidated %v, dispatched %d", q.providers, b.count())
	}
	snap, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Signals) != 1 || snap.Signals[0].Type != "webhook" || snap.Signals[0].LastID != "r1" {
		t.Fatalf("signal state: %+v", snap.Signals)
	}
	found := false
	for _, a := range snap.Audit {
		found = found || (a.Kind == "reset_signal" && strings.Contains(a.Message, "codex"))
	}
	if !found {
		t.Fatal("reset signal not audited")
	}
	// A follow-up re-check queries again but adds no audit entry.
	before := len(snap.Audit)
	if _, err := e.ApplyResetSignal(ctx, sig, true); err != nil {
		t.Fatal(err)
	}
	if snap, _ = e.Snapshot(ctx); len(snap.Audit) != before || len(q.providers) != 2 {
		t.Fatalf("follow-up: audit %d→%d, invalidations %v", before, len(snap.Audit), q.providers)
	}
}

func TestReceiveSignalCannotImpersonatePolledSource(t *testing.T) {
	e, _, _ := fixture(t)
	ctx := context.Background()
	if err := e.SaveSignalState(ctx, domain.SignalSourceState{ID: "codex-resets", Type: "codex-resets", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReceiveSignal(ctx, domain.ResetSignal{Source: "codex-resets", Provider: "codex", ID: "x"}); err == nil {
		t.Fatal("webhook overwrote a polled source")
	}
	for _, bad := range []domain.ResetSignal{{Source: "hook", Provider: "Open AI", ID: "x"}, {Source: "hook", Provider: "codex", ID: "a b"}} {
		if _, err := e.ReceiveSignal(ctx, bad); err == nil {
			t.Errorf("invalid signal accepted: %+v", bad)
		}
	}
}

func TestClientAndClaudeSessionsNeverShareARecord(t *testing.T) {
	e, _, _ := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	claude := domain.HookEvent{EventID: newID(), SessionID: "shared", HookEventName: "UserPromptSubmit", ObservedAt: now,
		Origin: &domain.SessionOrigin{App: domain.AppTerminal}}
	if err := e.HandleEvent(ctx, claude); err != nil {
		t.Fatal(err)
	}
	client := domain.HookEvent{EventID: "client:codex:1", SessionID: "shared", HookEventName: "StopFailure", Error: "rate_limit", ObservedAt: now.Add(time.Second),
		Kind: "code", Provider: "codex", Origin: &domain.SessionOrigin{App: domain.AppClient, Client: "codex"}}
	if err := e.HandleEvent(ctx, client); !errors.Is(err, errClientMismatch) {
		t.Fatalf("client event redirected a Claude session: %v", err)
	}
	if s := state(t, e, "shared"); s.Origin == nil || s.Origin.App == domain.AppClient || s.Provider != "" || s.State != domain.Running {
		t.Fatalf("session changed: %+v", s)
	}
	// A client's own session records its provider from the event.
	own := client
	own.EventID, own.SessionID = "client:codex:2", "thread-1"
	if err := e.HandleEvent(ctx, own); err != nil {
		t.Fatal(err)
	}
	if s := state(t, e, "thread-1"); s.Provider != "codex" || s.Origin == nil || s.Origin.Client != "codex" {
		t.Fatalf("client session: %+v", s)
	}
}
