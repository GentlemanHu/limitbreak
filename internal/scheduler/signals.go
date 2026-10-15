package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// SignalStates returns a copy of every persisted signal source state.
func (e *Engine) SignalStates(ctx context.Context) (map[string]domain.SignalSourceState, error) {
	if err := e.init(ctx); err != nil {
		return nil, err
	}
	t, err := e.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.SignalSourceState, len(t.Signals))
	for id, s := range t.Signals {
		out[id] = s
	}
	return out, nil
}

func (e *Engine) SaveSignalState(ctx context.Context, s domain.SignalSourceState) error {
	if s.ID == "" {
		return errors.New("signal source ID is required")
	}
	return e.store.Update(ctx, func(t *store.Tx) error {
		if t.Signals == nil {
			t.Signals = map[string]domain.SignalSourceState{}
		}
		t.Signals[s.ID] = s
		return nil
	})
}

// HasProvider reports whether any observed account belongs to provider.
func (e *Engine) HasProvider(ctx context.Context, provider string) (bool, error) {
	t, err := e.store.Read(ctx)
	if err != nil {
		return false, err
	}
	for _, a := range t.Accounts {
		if strings.EqualFold(a.Provider, provider) {
			return true, nil
		}
	}
	return false, nil
}

// ApplyResetSignal makes the provider's accounts due for a quota query and
// wakes the refresh loop. It never dispatches: a send still requires fresh
// quota evidence, so a wrong or early signal costs one extra query at most.
// A follow-up re-check is quiet; the first application is audited.
func (e *Engine) ApplyResetSignal(ctx context.Context, s domain.ResetSignal, followUp bool) (int, error) {
	s, err := domain.NormalizeSignal(s)
	if err != nil {
		return 0, err
	}
	if err := e.init(ctx); err != nil {
		return 0, err
	}
	n := 0
	if source, ok := e.quota.(interface {
		InvalidateProvider(context.Context, string) (int, error)
	}); ok {
		if n, err = source.InvalidateProvider(ctx, s.Provider); err != nil {
			return 0, err
		}
	}
	if !followUp {
		now := e.now()
		kind := "已执行"
		if s.Kind == domain.SignalResetScheduled {
			kind = "预定时间已到"
		}
		message := fmt.Sprintf("%s：%s 额度重置%s，%d 个账号提前重新查询额度（仅查询，发送仍需最新额度证据）", s.Source, s.Provider, kind, n)
		if err := e.store.Update(ctx, func(t *store.Tx) error {
			t.Log(now, "reset_signal", "", message)
			return nil
		}); err != nil {
			return n, err
		}
	}
	e.reschedule()
	return n, nil
}

// ReceiveSignal handles a pushed signal (webhook). The signal's Source names a
// webhook channel; it cannot reuse the ID of a polled source. A repeated ID
// on the same channel returns ErrDuplicateSignal without querying again.
func (e *Engine) ReceiveSignal(ctx context.Context, s domain.ResetSignal) (int, error) {
	s, err := domain.NormalizeSignal(s)
	if err != nil {
		return 0, err
	}
	if err := e.init(ctx); err != nil {
		return 0, err
	}
	now := e.now()
	s.ReceivedAt = now
	if s.At.IsZero() {
		s.At = now
	}
	duplicate := false
	err = e.store.Update(ctx, func(t *store.Tx) error {
		if t.Signals == nil {
			t.Signals = map[string]domain.SignalSourceState{}
		}
		state, exists := t.Signals[s.Source]
		if exists && state.Type != "webhook" {
			return fmt.Errorf("signal source %q is a polled source", s.Source)
		}
		if exists && state.LastID == s.ID {
			duplicate = true
			return nil
		}
		received := now
		signal := s
		t.Signals[s.Source] = domain.SignalSourceState{ID: s.Source, Type: "webhook", Name: "Webhook · " + s.Source, Provider: s.Provider,
			Enabled: true, State: domain.SignalStateOK, LastPollAt: &received, LastID: s.ID, Latest: &signal, LastTriggeredAt: &received}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if duplicate {
		return 0, domain.ErrDuplicateSignal
	}
	return e.ApplyResetSignal(ctx, s, false)
}

func sortedSignals(t *store.State) []domain.SignalSourceState {
	out := make([]domain.SignalSourceState, 0, len(t.Signals))
	for _, s := range t.Signals {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
