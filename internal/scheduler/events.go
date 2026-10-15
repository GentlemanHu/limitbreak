package scheduler

import (
	"context"
	"errors"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// IsRateLimit distinguishes exhaustion from authentication, overload and arbitrary failures.
func IsRateLimit(event domain.HookEvent) bool {
	code := strings.ToLower(strings.TrimSpace(event.Error))
	switch code {
	case "rate_limit_error", "rate_limit", "quota_exceeded", "usage_limit", "usage_limit_exceeded", "429":
		return true
	}
	text := strings.ToLower(event.Error + " " + event.ErrorDetails)
	for _, deny := range []string{"authentication", "unauthorized", "invalid api key", "permission denied", "overloaded", "529"} {
		if strings.Contains(text, deny) {
			return false
		}
	}
	for _, term := range []string{"rate limit", "rate_limit_error", "too many requests", "usage limit", "quota exceeded", "quota exhausted", "quota_exceeded", "额度耗尽", "使用限额", "达到使用上限"} {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func (e *Engine) HandleEvent(ctx context.Context, event domain.HookEvent) error {
	if event.EventID == "" || event.SessionID == "" || event.ObservedAt.IsZero() {
		return errors.New("hook event_id, session_id and observed_at are required")
	}
	if event.Origin != nil {
		// The host app is re-derived from the reported facts, so a label from an
		// older hook build cannot select a channel this version no longer has.
		origin := *event.Origin
		origin.App = domain.ClassifyOrigin(origin)
		if err := domain.ValidateOrigin(origin); err != nil {
			return err
		}
		event.Origin = &origin
	}
	switch event.HookEventName {
	case "SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "ModelChange", "PostModelSwitch":
	case "Notification":
		if !isQuotaNotification(event.NotificationType) {
			return errors.New("unsupported quota notification")
		}
	default:
		return errors.New("unsupported hook event")
	}
	if err := e.init(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	kept := false
	var superseded []string
	err := e.store.Update(ctx, func(t *store.Tx) error {
		accepted, err := t.AcceptEvent(event)
		if err != nil || !accepted {
			return err
		}
		changed = true
		if barrier := t.HookBarriers[event.SessionID]; !barrier.IsZero() && !event.ObservedAt.After(barrier) {
			changed = false
			return nil
		}
		now := e.now()
		s := t.Sessions[event.SessionID]
		if s.ID == "" {
			s = domain.Session{ID: event.SessionID, State: domain.Attention}
		}
		if event.HookEventName == "Notification" {
			t.Sessions[s.ID] = quotaNotification(t, s, event, now)
			return nil
		}
		if event.Origin != nil && s.Origin != nil && clientOf(*event.Origin) != clientOf(*s.Origin) {
			// A generic client and a Claude session (or two clients) never share
			// one record: a colliding ID must not redirect where a session resumes.
			return errClientMismatch
		}
		if event.Cwd != "" {
			s.Cwd = event.Cwd
		}
		if event.Origin != nil {
			origin := *event.Origin
			s.Origin = &origin
			superseded = detachSiblings(t, s.ID, origin, now)
		}
		if event.Kind == "code" || event.Kind == "chat" || event.Kind == "cowork" {
			s.Kind = event.Kind
		}
		if event.SessionTitle != "" {
			s.Title = event.SessionTitle
		}
		if event.Provider != "" && s.Origin != nil && s.Origin.App == domain.AppClient && domain.ValidSignalProvider(event.Provider) {
			s.Provider = event.Provider
		}
		s.UpdatedAt = event.ObservedAt
		var dispatched *domain.Job
		for _, j := range t.Jobs {
			if j.SessionID == s.ID && j.Generation == s.Generation && (j.State == domain.Dispatching || j.State == domain.Verifying) {
				copy := j
				dispatched = &copy
				break
			}
		}
		if dispatched != nil && !event.ObservedAt.After(t.DispatchStarted[dispatched.ID]) {
			changed = false
			return nil
		}
		if dispatched != nil && (event.HookEventName == "UserPromptSubmit" || event.HookEventName == "Stop" || event.HookEventName == "StopFailure" || event.HookEventName == "SessionEnd") {
			// An actual hook, not the accessibility return value, confirms submission.
			j := *dispatched
			j.State = domain.Completed
			j.LeaseUntil = nil
			j.UpdatedAt = now
			j.Reason = "confirmed by " + event.HookEventName
			t.Jobs[j.ID] = j
			if j.AccountID != "" {
				t.RunningAccounts[s.ID] = j.AccountID
			}
		}
		if event.HookEventName == "SessionEnd" && s.Origin != nil && s.Origin.App == domain.AppTerminal {
			// The CLI process is exiting; its pane must never be typed into again.
			origin := *s.Origin
			origin.Detached = true
			s.Origin = &origin
		}
		switch {
		case event.HookEventName == "SessionStart" && dispatched != nil && event.Source != "clear":
			// The same conversation's process (re)starting while a continue is in
			// flight: a headless resume, or Claude desktop spawning the session
			// process when its deep link reopens it. The dispatched job keeps
			// waiting for the prompt hook, and the in-flight send is not cancelled.
			kept = true
		case event.HookEventName == "SessionEnd" && dispatched == nil && keepAfterExit(t, s, event):
			s.State = domain.Cooldown
			s.Reason = "终端已关闭；额度恢复后将在后台续写"
			if s.Origin.App == domain.AppClaudeDesktop {
				s.Reason = "Claude 已结束该会话进程；额度恢复后将重新打开会话并继续"
			}
			if s.Origin.App == domain.AppClient {
				s.Reason = "客户端会话已退出；额度恢复后将通过配置的客户端命令继续"
			}
		case event.HookEventName == "SessionStart":
			delete(t.RunningAccounts, s.ID)
			invalidate(t, s.ID, "new session lifecycle", now)
			s.Generation++
			s.State = domain.Running
			s.Reason = "session started"
			s.Error = ""
			s.NativeResume = ""
		case event.HookEventName == "UserPromptSubmit":
			if dispatched == nil {
				delete(t.RunningAccounts, s.ID)
			}
			invalidate(t, s.ID, "new prompt superseded previous generation", now)
			s.Generation++
			s.State = domain.Running
			s.Reason = "prompt submitted"
			s.Error = ""
		case event.HookEventName == "Stop":
			invalidate(t, s.ID, "normal stop", now)
			s.State = domain.Completed
			s.Reason = "normal stop"
			s.Error = ""
			delete(t.RunningAccounts, s.ID)
		case event.HookEventName == "SessionEnd":
			invalidate(t, s.ID, "session ended", now)
			s.Generation++
			s.State = domain.Cancelled
			s.Reason = "session ended"
			if s.Origin != nil && s.Origin.App == domain.AppClaudeDesktop {
				// Claude ends idle desktop session processes on its own; the
				// session itself stays in the sidebar and reopens by its link.
				s.State = domain.Completed
				s.Reason = "Claude 已结束该会话进程；会话仍可重新打开"
			}
			s.Error = ""
			delete(t.RunningAccounts, s.ID)
		case event.HookEventName == "StopFailure":
			invalidate(t, s.ID, "failure superseded previous generation", now)
			s.Generation++
			delete(t.RunningAccounts, s.ID)
			s.Error = event.Error // Do not persist potentially sensitive ErrorDetails or prompts.
			if IsRateLimit(event) {
				t.QuotaFailures[s.ID] = event.ObservedAt
				s.State = domain.Cooldown
				s.Reason = "rate limit"
				enqueue(t, s, now)
			} else {
				s.State = domain.Attention
				s.Reason = "non-quota StopFailure; manual review required"
			}
		case event.HookEventName == "ModelChange" || event.HookEventName == "PostModelSwitch":
			if !legacyGuard(s) && (s.Binding == nil || len(s.Binding.MonitorResources) == 0) {
				// Hosted sessions recompute their pool from the model on every scan.
				break
			}
			invalidate(t, s.ID, "model changed; binding must be revalidated", now)
			if s.Binding != nil && len(s.Binding.MonitorResources) > 0 {
				copy := *s.Binding
				copy.AutoResume = false
				copy.ResumeResources = nil
				s.Binding = &copy
			} else {
				s.Binding = nil
			}
			// A model switch is not evidence that active work stopped. Preserve
			// running budget and any known account until Stop/StopFailure/cancel.
			s.Generation++
			s.State = domain.Attention
			s.Reason = "model changed; update explicit binding"
		}
		// Hook model metadata can invalidate a binding but can never create an account mapping.
		model := event.Model
		if event.NewModel != "" {
			model = event.NewModel
		}
		if model != "" {
			s.Model = model
			if legacyGuard(s) && s.Binding.AutoResume && s.Binding.Model != model && s.State != domain.Completed && s.State != domain.Cancelled {
				invalidate(t, s.ID, "hook model differs from binding", now)
				s.State = domain.Attention
				s.Reason = "hook model differs from explicit binding"
			}
		}
		switch event.HookEventName {
		case "SessionStart", "UserPromptSubmit":
			markRunning(t, s.ID, event.ObservedAt)
		case "Stop", "StopFailure", "SessionEnd":
			clearRunning(t, s.ID)
		}
		t.Sessions[s.ID] = s
		t.Log(now, "hook", s.ID, event.HookEventName)
		return nil
	})
	if err == nil && changed {
		if !kept {
			e.cancelActive(event.SessionID)
		}
		for _, id := range superseded {
			e.cancelActive(id)
		}
		e.signal()
	}
	return err
}

var errClientMismatch = errors.New("session belongs to another client")

// clientOf is the generic client that owns an origin, or "" for Claude hosts.
func clientOf(o domain.SessionOrigin) string {
	if o.App != domain.AppClient {
		return ""
	}
	return o.Client
}

// detachSiblings marks other terminal sessions recorded on the same claude
// process as no longer hosted by it: the process now runs session id (after
// /resume, /clear or a reused PID). It returns the sessions it changed.
func detachSiblings(t *store.Tx, id string, origin domain.SessionOrigin, now time.Time) []string {
	if origin.App != domain.AppTerminal || origin.ClaudePID <= 1 {
		return nil
	}
	var changed []string
	for key, other := range t.Sessions {
		o := other.Origin
		if key == id || o == nil || o.App != domain.AppTerminal || o.ClaudePID != origin.ClaudePID || o.Detached || o.ObservedAt.After(origin.ObservedAt) {
			continue
		}
		copy := *o
		copy.Detached = true
		other.Origin = &copy
		other.UpdatedAt = now
		t.Sessions[key] = other
		changed = append(changed, key)
	}
	return changed
}

// keepAfterExit preserves a queued quota continue when the session process
// exits normally: a terminal session is resumed headlessly and a desktop
// session is reopened by its link once quota recovers. /clear and logout are
// deliberate and cancel it as before.
func keepAfterExit(t *store.Tx, s domain.Session, event domain.HookEvent) bool {
	if s.Origin == nil || (s.Origin.App != domain.AppTerminal && s.Origin.App != domain.AppClaudeDesktop && s.Origin.App != domain.AppClient) || event.Reason == "clear" || event.Reason == "logout" {
		return false
	}
	for _, j := range t.Jobs {
		if j.SessionID == s.ID && j.Kind == "" && j.Generation == s.Generation && (j.State == domain.Cooldown || j.State == domain.Ready) {
			return true
		}
	}
	return false
}
