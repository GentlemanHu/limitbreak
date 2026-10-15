package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/adapters"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func affinityAccount(id string, at time.Time) domain.Account {
	a := allowed(id)
	a.CheckedAt = at
	return a
}
func affinityFailure(t *testing.T, e *Engine, id string, at time.Time) {
	t.Helper()
	if err := e.HandleEvent(context.Background(), domain.HookEvent{EventID: id + "/failure", SessionID: id, HookEventName: "StopFailure", Error: "rate_limit", Kind: "code", ObservedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func TestCollisionCannotBorrowAnotherSourceBindingEvenAfterResolutionOrRestart(t *testing.T) {
	ctx := context.Background()
	clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &fixtureQuota{accounts: []domain.Account{affinityAccount("legacy-normal", clock.Now())}}
	b := &fixtureQuota{accounts: []domain.Account{affinityAccount("x", clock.Now())}}
	c := &fixtureQuota{accounts: []domain.Account{affinityAccount("good", clock.Now())}}
	quota, err := adapters.NewAggregator([]adapters.SourceInstance{
		{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: a},
		{ID: "source-b", Type: "readonly-http", Quota: b},
		{ID: "source-c", Type: "readonly-http", Quota: c},
	}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}
	p := policy()
	p.MinSendIntervalSeconds = 30
	e := NewWithClock(db, quota, bridge, p, clock)
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	bindB := binding("b-session")
	bindB.AccountIDs = []string{"source-b::x"}
	bindC := binding("c-session")
	bindC.AccountIDs = []string{"source-c::good"}
	for _, bind := range []domain.Binding{bindB, bindC} {
		if err := e.Bind(ctx, bind); err != nil {
			t.Fatal(err)
		}
		affinityFailure(t, e, bind.SessionID, clock.Now())
	}
	clock.at = clock.at.Add(time.Second)
	a.accounts = []domain.Account{affinityAccount("source-b::x", clock.Now())}
	b.accounts = []domain.Account{affinityAccount("x", clock.Now())}
	c.accounts = []domain.Account{affinityAccount("good", clock.Now())}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 1 || state(t, e, "b-session").State != domain.Cooldown || state(t, e, "c-session").State != domain.Verifying {
		t.Fatal("collision allowed B or globally blocked unrelated C")
	}
	bridge.mu.Lock()
	selected := bridge.bindings[0].AccountIDs[0]
	bridge.mu.Unlock()
	if selected != "source-c::good" {
		t.Fatal("wrong source selected by colliding account ID")
	}

	// Once B removes x, only the legacy CPA account has this same final ID.
	// The existing B binding must not silently change its source identity.
	clock.at = clock.at.Add(time.Minute)
	b.accounts = []domain.Account{}
	a.accounts = []domain.Account{affinityAccount("source-b::x", clock.Now())}
	c.accounts = []domain.Account{affinityAccount("good", clock.Now())}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 1 || !strings.Contains(state(t, e, "b-session").Reason, "re-binding") {
		t.Fatal("old B binding borrowed healthy legacy CPA identity")
	}
	peer := NewWithClock(db, quota, bridge, p, clock)
	if err := peer.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := peer.Step(ctx); err != nil {
		t.Fatal(err)
	}
	persisted, err := db.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 1 || persisted.BoundSources["b-session"]["source-b::x"] != "source-b" {
		t.Fatal("restart lost explicit source affinity")
	}
	// Only a new explicit enrollment from verified inventory may change affinity.
	if err := e.Bind(ctx, bindB); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 2 {
		t.Fatal("explicit re-binding of the current verified identity did not work")
	}
}

func TestSourceAffinityIsRecheckedAfterPreflightBeforeResume(t *testing.T) {
	ctx := context.Background()
	clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, b := &fixtureQuota{}, &fixtureQuota{accounts: []domain.Account{affinityAccount("x", clock.Now())}}
	quota, err := adapters.NewAggregator([]adapters.SourceInstance{{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: a}, {ID: "source-b", Type: "readonly-http", Quota: b}}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}, probeGate: make(chan struct{}), probeEntered: make(chan struct{}, 1)}
	e := NewWithClock(db, quota, bridge, policy(), clock)
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	bind := binding("s")
	bind.AccountIDs = []string{"source-b::x"}
	if err := e.Bind(ctx, bind); err != nil {
		t.Fatal(err)
	}
	affinityFailure(t, e, "s", clock.Now())
	clock.at = clock.at.Add(time.Second)
	b.accounts = []domain.Account{affinityAccount("x", clock.Now())}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	if err := e.scan(ctx, &workers); err != nil {
		t.Fatal(err)
	}
	receive(t, bridge.probeEntered)
	// Keep logical time fixed while the worker is active; only inventory identity changes.
	a.accounts = []domain.Account{affinityAccount("source-b::x", clock.Now())}
	b.accounts = []domain.Account{}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	close(bridge.probeGate)
	workers.Wait()
	if bridge.count() != 0 || state(t, e, "s").State != domain.Attention {
		t.Fatal("preflight identity switch bypassed final source affinity check")
	}
}

func TestUnknownMultiSourceReferencesRemainMonitorableButCannotAuthorizeResume(t *testing.T) {
	ctx := context.Background()
	clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, b := &fixtureQuota{}, &fixtureQuota{}
	quota, err := adapters.NewAggregator([]adapters.SourceInstance{{ID: domain.DefaultSourceID, Type: "cliproxyapi", Quota: a}, {ID: "source-b", Type: "readonly-http", Quota: b}}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	bridge := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}
	e := NewWithClock(db, quota, bridge, policy(), clock)
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	bind := binding("s")
	bind.AccountIDs = []string{"source-b::future"}
	bind.MonitorResources = []domain.ResourceRef{{AccountID: "source-b::future"}}
	if err := e.Bind(ctx, bind); err != nil {
		t.Fatal(err)
	}
	affinityFailure(t, e, "s", clock.Now())
	clock.at = clock.at.Add(time.Second)
	b.accounts = []domain.Account{affinityAccount("future", clock.Now())}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	session := state(t, e, "s")
	if bridge.count() != 0 || session.Monitor == nil || session.Monitor.State != "allowed" || session.State != domain.Cooldown {
		t.Fatal("unknown reference was dropped or implicitly gained sending authority")
	}
	if err := e.Bind(ctx, bind); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 1 {
		t.Fatal("verified explicit enrollment was not accepted")
	}
}

func TestLegacySingleCPARawIDsAndUnmappedBindingsMigrateWithoutPrefixGuessing(t *testing.T) {
	e, q, bridge := fixture(t)
	ctx := context.Background()
	clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	e.clock = clock
	legacy := binding("legacy")
	legacy.AccountIDs = []string{"source-b::x"}
	if err := e.store.Update(ctx, func(tx *store.Tx) error {
		s := domain.Session{ID: "legacy", Kind: "code", State: domain.Cooldown, Generation: 1, Binding: &legacy}
		tx.Sessions[s.ID] = s
		tx.QuotaFailures[s.ID] = clock.Now()
		enqueue(tx, s, clock.Now())
		delete(tx.BoundSources, s.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	clock.at = clock.at.Add(time.Second)
	q.accounts = []domain.Account{affinityAccount("source-b::x", clock.Now())}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 1 {
		t.Fatal("legacy ID containing namespace punctuation was renamed or rebound")
	}
	stored, err := e.store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BoundSources["legacy"]["source-b::x"] != domain.DefaultSourceID {
		t.Fatal("legacy migration guessed a source from ID prefix")
	}
}
