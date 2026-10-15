package scheduler

import (
	"context"
	"testing"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestMonitorOnlyApprovalCannotReenrollResumeSource(t *testing.T) {
	e, _, bridge := fixture(t)
	ctx := context.Background()
	if _, err := e.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	account := allowed("a")
	account.SourceID = "source-a"
	if err := e.store.Update(ctx, func(tx *store.Tx) error {
		tx.Accounts = []domain.Account{account}
		tx.Capabilities.Sources = []domain.SourceHealth{{ID: "source-a", Type: "cliproxyapi", State: "healthy", Connected: true}, {ID: "source-b", Type: "readonly-http", State: "healthy", Connected: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Bind(ctx, binding("target")); err != nil {
		t.Fatal(err)
	}
	st, _ := e.store.Read(ctx)
	if st.BoundSources["target"]["a"] != "source-a" {
		t.Fatal("explicit source enrollment missing")
	}
	if err := e.store.Update(ctx, func(tx *store.Tx) error { tx.Accounts[0].SourceID = "source-b"; return nil }); err != nil {
		t.Fatal(err)
	}
	r := preparedControl(t, e, "monitor-with-replaced-source", store.ControlAction{Operation: "bind_monitor", SessionID: "target", ExpectedGeneration: st.Sessions["target"].Generation, Params: store.ControlParams{MonitorMode: "all", MonitorResources: []domain.ResourceRef{{AccountID: "a"}}}}, true)
	result, err := e.ExecuteControl(ctx, r)
	if err != nil || !result.OK {
		t.Fatalf("monitor-only control: %+v %v", result, err)
	}
	after, _ := e.store.Read(ctx)
	if after.BoundSources["target"]["a"] != "source-a" || boundSourceMatches(&store.Tx{State: after}, "target", after.Accounts[0]) || bridge.count() != 0 {
		t.Fatal("monitor-only approval changed resume authority")
	}
}
