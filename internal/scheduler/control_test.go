package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func preparedControl(t *testing.T, e *Engine, id string, action store.ControlAction, approve bool) store.ControlRequest {
	t.Helper()
	ctx := context.Background()
	if err := e.init(ctx); err != nil {
		t.Fatal(err)
	}
	now := e.now()
	var request store.ControlRequest
	err := e.store.Update(ctx, func(tx *store.Tx) error {
		g := store.ControlGrant{ID: "grant-" + id, TokenHash: store.ControlTokenHash("isolated-control-secret"), Scopes: []string{"read", store.ControlScope(action.Operation)}, SessionTargets: []string{action.SessionID}, ExpiresAt: now.Add(time.Hour)}
		if err := tx.AddControlGrant(g, now); err != nil {
			return err
		}
		var err error
		request, err = tx.CreateControlRequest(store.ControlRequest{ID: id, GrantID: g.ID, Action: action, ExpiresAt: now.Add(time.Minute)}, now)
		if err != nil || !approve {
			return err
		}
		request, err = tx.ApproveControl(request.ID, request.Digest, action.ExpectedGeneration, true, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestControlCancelClaimMutationAndReplayAreAtomic(t *testing.T) {
	e, _, b := fixture(t)
	ctx := context.Background()
	if err := e.Bind(ctx, binding("cancel-target")); err != nil {
		t.Fatal(err)
	}
	before, _ := e.store.Read(ctx)
	generation := before.Sessions["cancel-target"].Generation
	r := preparedControl(t, e, "cancel-once", store.ControlAction{Operation: "cancel", SessionID: "cancel-target", ExpectedGeneration: generation}, true)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := e.ExecuteControl(ctx, r)
			if err != nil || !result.OK || result.Generation != generation+1 || result.SendRetractionConfirmed {
				t.Errorf("control replay: %+v %v", result, err)
			}
		}()
	}
	wg.Wait()
	after, _ := e.store.Read(ctx)
	if s := after.Sessions["cancel-target"]; s.Generation != generation+1 || s.State != domain.Cancelled {
		t.Fatalf("replayed cancellation: %+v", s)
	}
	if after.ControlRequests[r.ID].State != store.ControlCompleted || b.count() != 0 {
		t.Fatal("cancel completion missing or performed send")
	}
}

func TestUnapprovedExpiredRevokedAndChangedGenerationCannotExecute(t *testing.T) {
	for _, scenario := range []string{"pending", "expired", "revoked", "generation"} {
		t.Run(scenario, func(t *testing.T) {
			e, _, _ := fixture(t)
			ctx := context.Background()
			if err := e.Bind(ctx, binding("target")); err != nil {
				t.Fatal(err)
			}
			st, _ := e.store.Read(ctx)
			gen := st.Sessions["target"].Generation
			r := preparedControl(t, e, "request", store.ControlAction{Operation: "cancel", SessionID: "target", ExpectedGeneration: gen}, scenario != "pending")
			if err := e.store.Update(ctx, func(tx *store.Tx) error {
				switch scenario {
				case "expired":
					v := tx.ControlRequests[r.ID]
					v.ExpiresAt = e.now().Add(-time.Second)
					tx.ControlRequests[r.ID] = v
				case "revoked":
					return tx.RevokeControlGrant(r.GrantID, e.now())
				case "generation":
					s := tx.Sessions["target"]
					s.Generation++
					tx.Sessions[s.ID] = s
					gen++
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			result, _ := e.ExecuteControl(ctx, r)
			after, _ := e.store.Read(ctx)
			if result.OK || after.Sessions["target"].Generation != gen || after.Sessions["target"].State == domain.Cancelled {
				t.Fatal("unapproved/stale control mutated session")
			}
		})
	}
}

func TestPolicyPatchCASPreservesOtherSettings(t *testing.T) {
	e, _, _ := fixture(t)
	ctx := context.Background()
	snap, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := e.PatchPolicy(ctx, snap.PolicyDigest, map[string]json.RawMessage{"max_running": json.RawMessage("3")})
	if err != nil || updated.MaxRunning != 3 || updated.ContinueMessage != snap.Policy.ContinueMessage {
		t.Fatalf("patch: %+v %v", updated, err)
	}
	if _, err := e.PatchPolicy(ctx, snap.PolicyDigest, map[string]json.RawMessage{"paused": json.RawMessage("true")}); !errors.Is(err, store.ErrControlConflict) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	for _, patch := range []map[string]json.RawMessage{{"continue_message": json.RawMessage("null")}, {"max_running": json.RawMessage("1.5")}, {"unknown_field": json.RawMessage("1")}, {"paused": json.RawMessage("null")}} {
		if _, err := e.PatchPolicy(ctx, store.PolicyDigest(updated), patch); err == nil {
			t.Fatal("invalid patch accepted")
		}
	}
	after, _ := e.Snapshot(ctx)
	if after.PolicyDigest != store.PolicyDigest(updated) || after.Policy.Paused {
		t.Fatal("failed patch partially mutated policy")
	}
}

func TestApprovedSettingsPatchCannotOverwriteLaterPolicy(t *testing.T) {
	e, _, _ := fixture(t)
	ctx := context.Background()
	snap, _ := e.Snapshot(ctx)
	r := preparedControl(t, e, "policy-request", store.ControlAction{Operation: "settingsPatch", SessionID: "daemon", PolicyDigest: snap.PolicyDigest, Params: store.ControlParams{SettingsPatch: map[string]json.RawMessage{"max_running": json.RawMessage("2")}}}, true)
	p := snap.Policy
	p.MaxPerAccount = 2
	if err := e.SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ExecuteControl(ctx, r); !errors.Is(err, store.ErrControlConflict) {
		t.Fatalf("stale approved policy: %v", err)
	}
	after, _ := e.Snapshot(ctx)
	if after.Policy.MaxPerAccount != 2 || after.Policy.MaxRunning != p.MaxRunning {
		t.Fatal("stale control overwrote policy")
	}
}

type countedControlQuota struct {
	fixtureQuota
	calls atomic.Int64
}

func (q *countedControlQuota) Refresh(ctx context.Context) ([]domain.Account, error) {
	q.calls.Add(1)
	return q.fixtureQuota.Refresh(ctx)
}

func TestApprovedRefreshIsNotReplayed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "failure", false: "success"}[fail], func(t *testing.T) {
			e, _, _ := fixture(t)
			q := &countedControlQuota{}
			if fail {
				q.err = errors.New("private upstream error")
			}
			e.quota = q
			r := preparedControl(t, e, "refresh-request", store.ControlAction{Operation: "refresh", SessionID: "daemon"}, true)
			for i := 0; i < 3; i++ {
				result, err := e.ExecuteControl(context.Background(), r)
				if err != nil || result.OK == fail {
					t.Fatalf("refresh: %+v %v", result, err)
				}
			}
			if q.calls.Load() != 1 {
				t.Fatal("refresh replayed upstream request")
			}
		})
	}
}

func TestRejectedControlDoesNotCancelActiveExecution(t *testing.T) {
	e, _, _ := fixture(t)
	ctx := context.Background()
	b := binding("target")
	b.ResumeOwner = "native"
	b.AutoResume = false
	b.MonitorResources = []domain.ResourceRef{{AccountID: "a"}}
	if err := e.Bind(ctx, b); err != nil {
		t.Fatal(err)
	}
	st, _ := e.store.Read(ctx)
	yes := true
	r := preparedControl(t, e, "invalid-enable", store.ControlAction{Operation: "enable_auto_resume", SessionID: "target", ExpectedGeneration: st.Sessions["target"].Generation, Params: store.ControlParams{Enabled: &yes}}, true)
	cancelled := false
	e.active["target"] = activeCall{cancel: func() { cancelled = true }}
	result, err := e.ExecuteControl(ctx, r)
	if err != nil || result.OK || cancelled {
		t.Fatalf("rejected control cancelled work: %+v %v", result, err)
	}
	after, _ := e.store.Read(ctx)
	if after.ControlRequests[r.ID].State != store.ControlCompleted || after.Sessions["target"].Generation != st.Sessions["target"].Generation {
		t.Fatal("rejected control mutated binding")
	}
}

func TestMonitorControlRequiresCurrentInventory(t *testing.T) {
	e, _, b := fixture(t)
	ctx := context.Background()
	if err := e.Bind(ctx, domain.Binding{SessionID: "target", MonitorResources: []domain.ResourceRef{{AccountID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	st, _ := e.store.Read(ctx)
	r := preparedControl(t, e, "monitor-request", store.ControlAction{Operation: "bind_monitor", SessionID: "target", ExpectedGeneration: st.Sessions["target"].Generation, Params: store.ControlParams{MonitorResources: []domain.ResourceRef{{AccountID: "absent"}}, MonitorMode: "all"}}, true)
	result, err := e.ExecuteControl(ctx, r)
	if err != nil || result.OK || b.count() != 0 {
		t.Fatalf("invented monitor resource accepted: %+v %v", result, err)
	}
}
