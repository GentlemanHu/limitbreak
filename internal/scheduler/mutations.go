package scheduler

import (
	"errors"
	"strings"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// The control plane and public methods share validation and transaction-only
// mutations. Callers own the store transaction and post-commit cancellation.
func validateBinding(b domain.Binding) error {
	if strings.TrimSpace(b.SessionID) == "" || len(b.SessionID) > 256 || strings.ContainsAny(b.SessionID, "/\x00\r\n") {
		return errors.New("binding session_id is invalid")
	}
	if err := domain.ValidateResumeFields(b); err != nil {
		return err
	}
	if b.AutoResume {
		if b.Kind != "code" || b.Provider == "" || b.Model == "" || len(b.AccountIDs) == 0 {
			return errors.New("automatic binding requires code kind, explicit provider, model and account IDs")
		}
		if !b.AcknowledgeSideEffects {
			return errors.New("automatic binding requires side-effect acknowledgement")
		}
		if b.TargetTitle == "" && b.TargetIdentifier == "" && b.DeepLink == "" {
			return errors.New("automatic binding requires an explicit desktop target")
		}
	}
	if err := validateResourceRefs(b); err != nil {
		return err
	}
	if len(b.AccountIDs) > 256 {
		return errors.New("binding account pool is too large")
	}
	ids := map[string]bool{}
	for _, id := range b.AccountIDs {
		if strings.TrimSpace(id) == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") || ids[id] {
			return errors.New("binding account IDs must be valid and unique")
		}
		ids[id] = true
	}
	return nil
}

func (e *Engine) bindInTx(t *store.Tx, b domain.Binding, now time.Time) error {
	if err := validateBinding(b); err != nil {
		return err
	}
	ensureLifecycle(t)
	b.ExecutionGeneration, b.ExecutionLease, b.ExecutionUserIdleSeconds = 0, "", 0
	b.ExecutionDirect, b.ExecutionRestoreFocus, b.ExecutionQuietSeconds = false, false, 0
	s := t.Sessions[b.SessionID]
	if s.ID == "" {
		s = domain.Session{ID: b.SessionID, State: domain.Attention, Reason: "awaiting a session hook"}
	}
	invalidate(t, s.ID, "binding changed", now)
	s.Generation++
	s.Binding = &b
	if b.AutoResume {
		t.BoundSources[s.ID] = e.captureSources(t, b)
	} else {
		delete(t.BoundSources, s.ID)
	}
	if !b.AutoResume && len(b.MonitorResources) == 0 && len(b.AccountIDs) == 0 {
		s.Binding = nil
	}
	if b.AutoResume && s.Kind == "" {
		s.Kind = b.Kind
	}
	s.UpdatedAt = now
	if s.State == domain.Dispatching || s.State == domain.Verifying {
		s.State = domain.Attention
		s.Reason = "binding changed during dispatch; confirm manually"
	}
	// Rebinding is the user's explicit ownership confirmation. Attention is not
	// automatically re-armed: only a subsequent lifecycle event may enqueue it.
	t.Sessions[s.ID] = s
	if s.State == domain.Cooldown || s.State == domain.Ready {
		enqueue(t, s, now)
	}
	t.Log(now, "binding", s.ID, "explicit binding saved")
	return nil
}

func cancelInTx(t *store.Tx, sessionID string, now time.Time) error {
	s, ok := t.Sessions[sessionID]
	if !ok {
		return errors.New("session not found")
	}
	ensureLifecycle(t)
	invalidate(t, sessionID, "cancelled by user", now)
	t.HookBarriers[sessionID] = now
	s.Generation++
	s.State, s.Reason, s.UpdatedAt = domain.Cancelled, "cancelled by user", now
	t.Sessions[sessionID] = s
	clearRunning(t, sessionID)
	t.Log(now, "cancel", sessionID, s.Reason)
	return nil
}

func setPolicyInTx(t *store.Tx, p domain.Policy, now time.Time) error {
	if err := config.ValidatePolicy(p); err != nil {
		return err
	}
	// Invalidate persisted claims as well as live contexts. A paused/unpaused
	// policy or changed global text can never revive an old IPC send closure.
	for id, s := range t.Sessions {
		if s.State != domain.Dispatching && s.State != domain.Verifying {
			continue
		}
		invalidate(t, id, "policy changed during dispatch; confirm manually", now)
		s.Generation++
		s.State = domain.Attention
		s.Reason = "policy changed during dispatch; confirm manually"
		s.UpdatedAt = now
		t.Sessions[id] = s
	}
	t.Policy = &p
	t.Log(now, "policy", "", "scheduling policy saved")
	return nil
}
