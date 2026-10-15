package scheduler

import (
	"context"
	"testing"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func TestConfirmedSessionControlsApplyOnce(t *testing.T) {
	lb := &locatingBridge{fixtureBridge: &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}}
	e, _, clock := hostedEngine(t, lb, false)
	ctx := context.Background()
	hostedEvent(t, e, "s", "Stop", clock.at)
	state, _ := e.store.Read(ctx)
	generation := state.Sessions["s"].Generation

	priority, text := 7, "请继续当前任务"
	settings := domain.SessionSettings{Priority: &priority, ContinueMessage: &text}
	r := preparedControl(t, e, "settings-1", store.ControlAction{Operation: "session_settings", SessionID: "s", ExpectedGeneration: generation,
		Params: store.ControlParams{SessionSettings: &settings}}, true)
	if result, err := e.ExecuteControl(ctx, r); err != nil || !result.OK {
		t.Fatalf("settings control: %+v %v", result, err)
	}
	state, _ = e.store.Read(ctx)
	if s := state.Sessions["s"]; s.Priority != 7 || s.ContinueMessage != text || s.Generation != generation {
		t.Fatalf("settings not applied exactly: %+v", s)
	}

	r = preparedControl(t, e, "locate-1", store.ControlAction{Operation: "locate", SessionID: "s", ExpectedGeneration: generation}, true)
	if result, err := e.ExecuteControl(ctx, r); err != nil || !result.OK || result.Code != "located" || len(lb.located) != 1 || lb.count() != 0 {
		t.Fatalf("locate control: %+v %v", result, err)
	}
	if result, _ := e.ExecuteControl(ctx, r); len(lb.located) != 1 || result.Code != "located" {
		t.Fatal("locate replayed")
	}

	r = preparedControl(t, e, "test-1", store.ControlAction{Operation: "test_send", SessionID: "s", ExpectedGeneration: generation}, true)
	if result, err := e.ExecuteControl(ctx, r); err != nil || !result.OK {
		t.Fatalf("test-send control: %+v %v", result, err)
	}
	state, _ = e.store.Read(ctx)
	queued := 0
	for _, j := range state.Jobs {
		if j.SessionID == "s" && j.Kind == "test" && j.State == domain.Ready && j.Message == text {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("test jobs queued = %d", queued)
	}
	// A second confirmed test while one is queued is refused, not stacked.
	r = preparedControl(t, e, "test-2", store.ControlAction{Operation: "test_send", SessionID: "s", ExpectedGeneration: generation}, true)
	if result, err := e.ExecuteControl(ctx, r); err != nil || result.OK || result.Code != "control_rejected" {
		t.Fatalf("stacked test-send: %+v %v", result, err)
	}
}

func TestSessionControlValidation(t *testing.T) {
	empty := domain.SessionSettings{}
	bad := "x\ny"
	for _, a := range []store.ControlAction{
		{Operation: "session_settings", SessionID: "s", ExpectedGeneration: 1},
		{Operation: "session_settings", SessionID: "s", ExpectedGeneration: 1, Params: store.ControlParams{SessionSettings: &empty}},
		{Operation: "session_settings", SessionID: "s", ExpectedGeneration: 1, Params: store.ControlParams{SessionSettings: &domain.SessionSettings{ContinueMessage: &bad}}},
		{Operation: "test_send", SessionID: "s", ExpectedGeneration: 1, Params: store.ControlParams{Message: "a\nb"}},
		{Operation: "locate", SessionID: "s", ExpectedGeneration: 1, Params: store.ControlParams{Message: "继续"}},
		{Operation: "locate", SessionID: "daemon"},
	} {
		if store.ValidateControlAction(a) == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}
