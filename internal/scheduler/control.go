package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// Approval, claim, mutation and outcome share one transaction for local controls.
// Refresh has an irrevocable claim before networking and is never replayed.
func (e *Engine) ExecuteControl(ctx context.Context, request store.ControlRequest) (store.ControlResult, error) {
	if err := e.init(ctx); err != nil {
		return store.ControlResult{}, err
	}
	e.policyMu.Lock()
	e.mu.Lock()
	var result store.ControlResult
	var action store.ControlAction
	var changedPolicy *domain.Policy
	claimed, refresh, locate, settingsOnly := false, false, false, false
	err := e.store.Update(ctx, func(t *store.Tx) error {
		stored, take, err := t.ClaimControl(request.ID, request.Digest, e.now())
		if err != nil {
			return err
		}
		if !take {
			if stored.Result != nil {
				result = *stored.Result
			} else {
				result.Code = "not_executed"
			}
			return nil
		}
		claimed = true
		action = stored.Action
		if action.Operation == "refresh" || action.Operation == "locate" {
			// Both reach outside the store; the claim is irrevocable and never replayed.
			refresh, locate = action.Operation == "refresh", action.Operation == "locate"
			return nil
		}
		result = store.ControlResult{OK: true, Code: "completed"}
		now := e.now()
		switch action.Operation {
		case "cancel":
			err = cancelInTx(t, action.SessionID, now)
		case "bind_monitor":
			for _, ref := range action.Params.MonitorResources {
				if !inventoryReference(t.Accounts, ref) {
					err = store.ErrControlInvalid
					break
				}
			}
			if err == nil {
				b := domain.Binding{SessionID: action.SessionID}
				if s := t.Sessions[action.SessionID]; s.Binding != nil {
					b = *s.Binding
				}
				b.MonitorResources, b.MonitorMode = action.Params.MonitorResources, action.Params.MonitorMode
				oldAffinity := t.BoundSources[action.SessionID]
				err = e.bindInTx(t, b, now)
				if err == nil && b.AutoResume {
					// Monitor-only approval cannot re-enroll routing authority when
					// an account's source changed since the explicit app binding.
					t.BoundSources[action.SessionID] = oldAffinity
				}
			}
		case "enable_auto_resume":
			s := t.Sessions[action.SessionID]
			if s.Binding == nil || action.Params.Enabled == nil {
				err = store.ErrControlInvalid
				break
			}
			b := *s.Binding
			if *action.Params.Enabled {
				if b.ResumeOwner != "guard" || !b.AcknowledgeSideEffects || !b.NativeAutoResumeDisabledAcknowledged {
					err = store.ErrControlForbidden
					break
				}
				for _, id := range b.AccountIDs {
					found := 0
					for _, account := range t.Accounts {
						if account.ID == id && domain.SourceConnected(account, t.Capabilities) && boundSourceMatches(t, s.ID, account) {
							found++
						}
					}
					if found != 1 {
						err = store.ErrControlForbidden
						break
					}
				}
			}
			if err == nil {
				oldAffinity := t.BoundSources[s.ID]
				b.AutoResume = *action.Params.Enabled
				err = e.bindInTx(t, b, now)
				// Disabling via a confirmed control does not erase the established owner.
				if err == nil && !b.AutoResume && oldAffinity != nil {
					t.BoundSources[s.ID] = oldAffinity
				}
			}
		case "session_settings":
			_, err = applySettingsInTx(t, action.SessionID, *action.Params.SessionSettings, now)
			settingsOnly = true
		case "test_send":
			_, err = queueTestInTx(t, action.SessionID, action.Params.Message, now)
		case "pause", "unpause", "settingsPatch":
			if t.Policy == nil {
				return store.ErrControlConflict
			}
			p := *t.Policy
			if action.Operation == "settingsPatch" {
				p, err = patchedPolicy(p, action.Params.SettingsPatch, now)
			} else {
				p.Paused = action.Operation == "pause"
			}
			if err == nil {
				err = setPolicyInTx(t, p, now)
				if err == nil {
					changedPolicy = &p
				}
			}
		default:
			err = store.ErrControlInvalid
		}
		if err != nil {
			result = store.ControlResult{Code: "control_rejected"}
		}
		if s, ok := t.Sessions[action.SessionID]; ok {
			result.Generation = s.Generation
		}
		return t.FinishControl(stored.ID, stored.Digest, result, now)
	})
	if err == nil && claimed && !refresh && !locate && result.OK {
		// Settings never interrupt an in-flight send; its heartbeat re-validates.
		if changedPolicy != nil {
			for _, active := range e.active {
				active.cancel()
			}
		} else if !settingsOnly && action.Operation != "test_send" {
			e.cancelActive(action.SessionID)
		}
		e.signal()
		e.reschedule()
	}
	e.mu.Unlock()
	if err == nil && changedPolicy != nil {
		if aware, ok := e.quota.(interface {
			SetPolicy(context.Context, domain.Policy) error
		}); ok {
			if updateErr := aware.SetPolicy(ctx, *changedPolicy); updateErr != nil {
				_ = e.recordError(ctx, "policy_adapter", errors.New("quota policy update unavailable"))
			}
		}
	}
	e.policyMu.Unlock()
	if err != nil || !(refresh || locate) || !claimed {
		return result, err
	}

	if locate {
		located, _ := e.Locate(ctx, action.SessionID)
		result = store.ControlResult{OK: located.OK, Code: "located"}
		if !result.OK {
			result.Code = "locate_failed"
		}
	} else {
		refreshErr := e.RefreshNow(ctx)
		result = store.ControlResult{OK: refreshErr == nil, Code: "refresh_completed"}
		if refreshErr != nil {
			result.Code = "refresh_failed"
		}
	}
	finishCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.store.Update(finishCtx, func(t *store.Tx) error { return t.FinishControl(request.ID, request.Digest, result, e.now()) }); err != nil {
		return store.ControlResult{Code: "execution_uncertain"}, err
	}
	return result, nil
}

func inventoryReference(accounts []domain.Account, ref domain.ResourceRef) bool {
	var selected *domain.Account
	for _, account := range accounts {
		if account.ID == ref.AccountID {
			if selected != nil {
				return false
			}
			copy := account
			selected = &copy
		}
	}
	if selected == nil {
		return false
	}
	windows := selected.Windows
	if ref.GroupID != "" {
		found := false
		for _, group := range selected.Groups {
			if group.ID == ref.GroupID {
				if found {
					return false
				}
				found = true
				windows = group.Windows
			}
		}
		if !found {
			return false
		}
	}
	if ref.WindowID == "" {
		return true
	}
	matches := 0
	for _, window := range windows {
		if window.ID == ref.WindowID {
			matches++
		}
	}
	return matches == 1
}

func patchedPolicy(current domain.Policy, patch map[string]json.RawMessage, now time.Time) (domain.Policy, error) {
	if len(patch) == 0 || len(patch) > 32 {
		return current, store.ErrControlInvalid
	}
	data, err := json.Marshal(current)
	if err != nil {
		return current, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return current, err
	}
	for key, value := range patch {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return current, store.ErrControlInvalid
		}
		switch key {
		case "paused":
			var v bool
			if json.Unmarshal(value, &v) != nil {
				return current, store.ErrControlInvalid
			}
		case "continue_message", "default_host_mode", "direct_delivery", "reset_fallback", "restore_focus", "claude_quiet":
			var v string
			if json.Unmarshal(value, &v) != nil {
				return current, store.ErrControlInvalid
			}
		case "model_aliases":
			var v map[string]string
			if json.Unmarshal(value, &v) != nil {
				return current, store.ErrControlInvalid
			}
		case "automation_consent":
			// Consent is recorded as a server timestamp, never a client-supplied one.
			var v bool
			if json.Unmarshal(value, &v) != nil {
				return current, store.ErrControlInvalid
			}
			delete(object, "automation_consent_at")
			if v {
				stamp, _ := json.Marshal(now.UTC())
				object["automation_consent_at"] = stamp
			}
			continue
		case "max_running", "max_per_account", "safety_seconds", "freshness_seconds", "verify_seconds", "long_probe_seconds", "unknown_probe_seconds", "min_send_interval_seconds", "early_probe_seconds", "near_reset_probe_seconds", "reset_lookahead_seconds", "user_idle_seconds", "running_stale_seconds", "queue_aging_seconds", "max_auto_resumes_per_hour", "native_grace_seconds", "claude_quiet_seconds":
			var v int
			if json.Unmarshal(value, &v) != nil {
				return current, store.ErrControlInvalid
			}
		default:
			return current, store.ErrControlInvalid
		}
		object[key] = value
	}
	data, err = json.Marshal(object)
	if err != nil {
		return current, err
	}
	var updated domain.Policy
	if err := json.Unmarshal(data, &updated); err != nil {
		return current, err
	}
	if err := config.ValidatePolicy(updated); err != nil {
		return current, store.ErrControlInvalid
	}
	return updated, nil
}

func (e *Engine) PatchPolicy(ctx context.Context, expected string, patch map[string]json.RawMessage) (domain.Policy, error) {
	if err := e.init(ctx); err != nil {
		return domain.Policy{}, err
	}
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.mu.Lock()
	var updated domain.Policy
	err := e.store.Update(ctx, func(t *store.Tx) error {
		if t.Policy == nil || expected == "" || store.PolicyDigest(*t.Policy) != expected {
			return store.ErrControlConflict
		}
		var err error
		updated, err = patchedPolicy(*t.Policy, patch, e.now())
		if err != nil {
			return err
		}
		return setPolicyInTx(t, updated, e.now())
	})
	if err == nil {
		for _, active := range e.active {
			active.cancel()
		}
		e.signal()
		e.reschedule()
	}
	e.mu.Unlock()
	if err == nil {
		if aware, ok := e.quota.(interface {
			SetPolicy(context.Context, domain.Policy) error
		}); ok {
			err = aware.SetPolicy(ctx, updated)
		}
	}
	return updated, err
}
