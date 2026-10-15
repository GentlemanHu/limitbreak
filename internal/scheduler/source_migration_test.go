package scheduler

import (
	"context"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestLegacyMultiSourceBindingRequiresExplicitEnrollment(t *testing.T) {
	ctx := context.Background()
	clock := &manualClock{at: time.Date(2025, 1, 1, 0, 0, 1, 0, time.UTC)}
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	account := affinityAccount("mirror::x", clock.Now())
	account.SourceID = domain.DefaultSourceID
	caps := domain.Capabilities{ProxyConnected: true, Sources: []domain.SourceHealth{
		{ID: domain.DefaultSourceID, Type: "cliproxyapi", Connected: true, State: "healthy"},
		{ID: "mirror", Type: "readonly-http", Connected: true, State: "healthy"},
	}}
	quota := &healthQuota{accounts: []domain.Account{account}, caps: caps}
	bridge := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}
	e := NewWithClock(db, quota, bridge, policy(), clock)
	bind := binding("legacy-multi")
	bind.AccountIDs = []string{account.ID}
	// An old multi-source database has no trustworthy binding-to-source map.
	// Its retained account may already have overwritten a colliding identity.
	if err := db.Update(ctx, func(tx *store.Tx) error {
		s := domain.Session{ID: bind.SessionID, Kind: "code", State: domain.Cooldown, Generation: 1, Binding: &bind}
		tx.Sessions[s.ID] = s
		tx.Accounts = []domain.Account{account}
		tx.Capabilities = caps
		tx.QuotaFailures[s.ID] = clock.Now().Add(-time.Second)
		enqueue(tx, s, clock.Now())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BoundSources[bind.SessionID][account.ID] != "" || bridge.count() != 0 {
		t.Fatal("old multi-source binding gained implicit authority from retained inventory")
	}
	if err := e.Bind(ctx, bind); err != nil {
		t.Fatal(err)
	}
	if err := e.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.count() != 1 {
		t.Fatal("explicit enrollment after verified collection was rejected")
	}
}

func TestSingleNonCPASourceCannotUseLegacyDefaultEnrollment(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	quota := &healthQuota{caps: domain.Capabilities{Sources: []domain.SourceHealth{
		{ID: domain.DefaultSourceID, Type: "readonly-http"},
	}}}
	e := New(db, quota, &fixtureBridge{}, policy())
	bind := binding("unknown")
	if err := e.Bind(ctx, bind); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.BoundSources[bind.SessionID][bind.AccountIDs[0]] != "" {
		t.Fatal("non-CPA source received the legacy default CPA exception")
	}
}

func TestRemovingSourceCannotRestoreImplicitLegacyEnrollment(t *testing.T) {
	bind := binding("removed-source")
	account := allowed(bind.AccountIDs[0])
	account.SourceID = domain.DefaultSourceID
	current := domain.SourceHealth{ID: domain.DefaultSourceID, Type: "cliproxyapi", Connected: true, State: "healthy"}
	e := &Engine{initialSources: []domain.SourceHealth{current}}
	tx := &store.Tx{State: &store.State{
		Sessions: map[string]domain.Session{bind.SessionID: {ID: bind.SessionID, Binding: &bind}},
		Accounts: []domain.Account{account},
		Capabilities: domain.Capabilities{ProxyConnected: true, Sources: []domain.SourceHealth{
			current, {ID: "mirror", Type: "readonly-http", Connected: true, State: "healthy"},
		}},
	}}
	e.migrateSources(tx)
	if tx.BoundSources[bind.SessionID][account.ID] != "" {
		t.Fatal("removing a source converted a legacy multi-source binding to default CPA authority")
	}
}
