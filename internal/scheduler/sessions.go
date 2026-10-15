package scheduler

import (
	"context"
	"sort"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func busy(state string) bool {
	return state == domain.Running || state == domain.Dispatching || state == domain.Verifying
}

// UpdateSessionSettings applies per-session hosting settings. A claim in flight
// is cancelled by the dispatch heartbeat if its target or text drifts.
func (e *Engine) UpdateSessionSettings(ctx context.Context, id string, settings domain.SessionSettings) (domain.Session, error) {
	if err := domain.ValidateSessionSettings(settings); err != nil {
		return domain.Session{}, domain.UserError(err.Error())
	}
	if err := e.init(ctx); err != nil {
		return domain.Session{}, err
	}
	var updated domain.Session
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.store.Update(ctx, func(t *store.Tx) error {
		var err error
		updated, err = applySettingsInTx(t, id, settings, e.now())
		return err
	})
	if err == nil {
		e.signal()
	}
	return updated, err
}

func applySettingsInTx(t *store.Tx, id string, settings domain.SessionSettings, now time.Time) (domain.Session, error) {
	s, ok := t.Sessions[id]
	if !ok {
		return domain.Session{}, domain.ErrSessionNotFound
	}
	settings.Apply(&s)
	t.Sessions[id] = s
	t.Log(now, "session_settings", id, "会话托管设置已保存")
	return s, nil
}

// TestSend queues one explicit user-initiated delivery. It skips quota and the
// hourly breaker but keeps the delivery lease, idle gate and hook confirmation.
func (e *Engine) TestSend(ctx context.Context, id, message string) (domain.Job, error) {
	if err := e.init(ctx); err != nil {
		return domain.Job{}, err
	}
	var job domain.Job
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.store.Update(ctx, func(t *store.Tx) error {
		var err error
		job, err = queueTestInTx(t, id, message, e.now())
		return err
	})
	if err == nil {
		e.signal()
	}
	return job, err
}

func queueTestInTx(t *store.Tx, id, message string, now time.Time) (domain.Job, error) {
	s, ok := t.Sessions[id]
	if !ok {
		return domain.Job{}, domain.ErrSessionNotFound
	}
	if busy(s.State) {
		return domain.Job{}, domain.UserError("会话正在运行或发送中，不能测试发送")
	}
	for _, j := range t.Jobs {
		if j.SessionID == id && j.Kind == "test" && (j.State == domain.Ready || j.State == domain.Dispatching || j.State == domain.Verifying) {
			return domain.Job{}, domain.UserError("该会话已有测试发送在进行")
		}
	}
	policy := config.EffectivePolicy(*t.Policy)
	if message == "" {
		message = s.ContinueMessage
	}
	if message == "" {
		message = policy.ContinueMessage
	}
	if !domain.ValidContinueMessage(message) {
		return domain.Job{}, domain.UserError(reasonBadMessage)
	}
	job := domain.Job{ID: s.ID + "/test-" + newID()[:12], SessionID: s.ID, Generation: s.Generation, State: domain.Ready, Kind: "test",
		NotBefore: now, Message: message, MessageDigest: digestText(message), Reason: "手动测试发送排队中", CreatedAt: now, UpdatedAt: now}
	if _, problem := jobBinding(t.Accounts, s, job, policy); problem != "" {
		return domain.Job{}, domain.UserError(problem)
	}
	invalidate(t, s.ID, "已被手动测试发送取代", now)
	t.Jobs[job.ID] = job
	s.State, s.Reason, s.UpdatedAt = domain.Ready, job.Reason, now
	t.Sessions[s.ID] = s
	t.Log(now, "test_send", s.ID, "手动测试发送已排队")
	return job, nil
}

// claimTest picks the oldest due test job whose session is still idle.
func claimTest(t *store.Tx, policy domain.Policy, now time.Time) (domain.Job, domain.Binding, bool) {
	var due []domain.Job
	for _, j := range t.Jobs {
		if j.Kind == "test" && j.State == domain.Ready && !j.NotBefore.After(now) {
			due = append(due, j)
		}
	}
	sort.Slice(due, func(a, b int) bool {
		if !due[a].CreatedAt.Equal(due[b].CreatedAt) {
			return due[a].CreatedAt.Before(due[b].CreatedAt)
		}
		return due[a].ID < due[b].ID
	})
	for _, j := range due {
		s, ok := t.Sessions[j.SessionID]
		if !ok || s.Generation != j.Generation || busy(s.State) {
			j.State, j.Reason, j.UpdatedAt = domain.Cancelled, "obsolete generation", now
			t.Jobs[j.ID] = j
			continue
		}
		b, problem := jobBinding(t.Accounts, s, j, policy)
		if problem == "" && !domain.ValidContinueMessage(j.Message) {
			problem = reasonBadMessage
		}
		if problem != "" {
			j.State, j.Reason, j.UpdatedAt = domain.Attention, problem, now
			t.Jobs[j.ID] = j
			s.State, s.Reason, s.UpdatedAt = domain.Attention, problem, now
			t.Sessions[s.ID] = s
			continue
		}
		return j, b, true
	}
	return domain.Job{}, domain.Binding{}, false
}

// Locate brings the session's host window or tab to the front without typing.
func (e *Engine) Locate(ctx context.Context, id string) (domain.BridgeResult, error) {
	locator, ok := e.bridge.(interface {
		Locate(context.Context, domain.Binding) domain.BridgeResult
	})
	if !ok {
		return domain.BridgeResult{Code: "locate_unavailable"}, domain.UserError("当前执行器不支持定位")
	}
	state, err := e.store.Read(ctx)
	if err != nil {
		return domain.BridgeResult{}, err
	}
	s, found := state.Sessions[id]
	if !found {
		return domain.BridgeResult{}, domain.ErrSessionNotFound
	}
	b, problem := resolveTarget(s)
	if problem != "" && legacyGuard(s) {
		b, problem = *s.Binding, ""
	}
	if problem != "" {
		return domain.BridgeResult{Code: "target_unresolved", Message: problem}, domain.UserError(problem)
	}
	if state.DesktopOwner != "" && state.DesktopUntil.After(e.now()) {
		return domain.BridgeResult{Code: "busy"}, domain.UserError("正在向其它会话发送，请稍后再定位")
	}
	return locator.Locate(ctx, b), nil
}
