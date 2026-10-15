package scheduler

import (
	"fmt"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func ensureLifecycle(t *store.Tx) {
	if t.ResumeHistory == nil {
		t.ResumeHistory = map[string][]time.Time{}
	}
	if t.RunningSince == nil {
		t.RunningSince = map[string]time.Time{}
	}
	for id, session := range t.Sessions {
		if session.State == domain.Running {
			if _, ok := t.RunningSince[id]; !ok {
				t.RunningSince[id] = session.UpdatedAt
			}
		}
	}
}
func clearRunning(t *store.Tx, id string) {
	delete(t.RunningSince, id)
	delete(t.RunningAccounts, id)
}
func markRunning(t *store.Tx, id string, at time.Time) {
	ensureLifecycle(t)
	t.RunningSince[id] = at
}

func resumesInHour(t *store.Tx, id string, now time.Time) int {
	ensureLifecycle(t)
	cutoff := now.Add(-time.Hour)
	kept := t.ResumeHistory[id][:0]
	for _, at := range t.ResumeHistory[id] {
		// Future timestamps survive clock rollback and conservatively consume
		// budget until real time catches up; no clock jump resets the breaker.
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	t.ResumeHistory[id] = kept
	return len(kept)
}
func resumeLimitReached(t *store.Tx, id string, now time.Time) bool {
	return resumesInHour(t, id, now) >= config.EffectivePolicy(*t.Policy).MaxAutoResumesPerHour
}
func recordResume(t *store.Tx, id string, at time.Time) {
	resumesInHour(t, id, at)
	t.ResumeHistory[id] = append(t.ResumeHistory[id], at)
}

// unrecordResume releases a reservation whose send provably never happened.
func unrecordResume(t *store.Tx, id string, at time.Time) {
	history := t.ResumeHistory[id]
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Equal(at) {
			t.ResumeHistory[id] = append(history[:i:i], history[i+1:]...)
			return
		}
	}
}

const (
	reasonStaleRunning  = "running lifecycle is stale/unknown; budget retained until confirmed stop or cancel"
	reasonProcessEnded  = "Claude 会话进程已结束，释放并发名额"
	reasonConcurrencyAt = "并发已满：%d 个自动继续的会话仍在运行（上限 %d），结束后立即继续"
)

// ProcessLookup reports whether pid is alive and when it started. Production
// wires the kernel process table; without it no process is presumed ended.
type ProcessLookup func(pid int) (start time.Time, alive bool)

func markStaleRunning(t *store.Tx, now time.Time) {
	ensureLifecycle(t)
	limit := time.Duration(config.EffectivePolicy(*t.Policy).RunningStaleSeconds) * time.Second
	for id, since := range t.RunningSince {
		s := t.Sessions[id]
		if s.State != domain.Running || (!since.IsZero() && now.Sub(since) < limit) {
			continue
		}
		s.State = domain.Attention
		s.Reason = reasonStaleRunning
		s.UpdatedAt = now
		t.Sessions[id] = s
		// Keep both markers. Missing events are not proof that work stopped.
		t.Log(now, "running_stale", id, s.Reason)
	}
}

// releaseEndedRunning frees the budget of sessions whose recorded Claude process
// is provably gone: detached, no longer in the process table, or its PID now
// belongs to a process started after the observation. A live process, or no
// recorded process at all, keeps the conservative reservation.
func releaseEndedRunning(t *store.Tx, lookup ProcessLookup, now time.Time) {
	if lookup == nil {
		return
	}
	ensureLifecycle(t)
	for id := range t.RunningSince {
		s, ok := t.Sessions[id]
		if !ok || s.Origin == nil {
			continue
		}
		if !s.Origin.Detached {
			if s.Origin.ClaudePID <= 1 {
				continue
			}
			if start, alive := lookup(s.Origin.ClaudePID); alive && !start.After(s.Origin.ObservedAt) {
				continue
			}
		}
		clearRunning(t, id)
		if s.State == domain.Running || (s.State == domain.Attention && s.Reason == reasonStaleRunning) {
			s.State, s.Reason, s.UpdatedAt = domain.Completed, reasonProcessEnded, now
			t.Sessions[id] = s
		}
		t.Log(now, "running_released", id, reasonProcessEnded)
	}
}

// noteConcurrencyWait makes a full global budget visible on every due session
// instead of leaving an expired countdown without explanation.
func noteConcurrencyWait(t *store.Tx, used, limit int, now time.Time) {
	reason := fmt.Sprintf(reasonConcurrencyAt, used, limit)
	for _, j := range t.Jobs {
		if j.Kind != "" || (j.State != domain.Cooldown && j.State != domain.Ready) || j.NotBefore.After(now) {
			continue
		}
		s, ok := t.Sessions[j.SessionID]
		if !ok || s.Generation != j.Generation || s.Reason == reason {
			continue
		}
		s.Reason = reason
		t.Sessions[s.ID] = s
	}
}

func isQuotaNotification(kind string) bool {
	switch kind {
	case "quota_auto_resume_fired", "quota_auto_resume_stale", "quota_auto_resume_disabled":
		return true
	}
	return false
}
func quotaNotification(t *store.Tx, s domain.Session, event domain.HookEvent, now time.Time) domain.Session {
	if !legacyGuard(s) && s.Binding == nil {
		return hostedNotification(t, s, event, now)
	}
	invalidate(t, s.ID, "native quota notification superseded external resume", now)
	s.Generation++
	t.HookBarriers[s.ID] = event.ObservedAt
	if s.Binding != nil {
		copy := *s.Binding
		copy.AutoResume = false
		copy.NativeAutoResumeDisabledAcknowledged = false
		if event.NotificationType == "quota_auto_resume_fired" {
			copy.ResumeOwner = "native"
		}
		s.Binding = &copy
	}
	s.Error = ""
	s.UpdatedAt = event.ObservedAt
	s.State = domain.Attention
	s.Reason = "native quota auto-resume requires manual/idle confirmation; no external takeover"
	switch event.NotificationType {
	case "quota_auto_resume_fired":
		recordResume(t, s.ID, event.ObservedAt)
		markRunning(t, s.ID, event.ObservedAt)
		s.State = domain.Running
		s.Reason = "native quota auto-resume fired; external resume invalidated"
	case "quota_auto_resume_stale":
		s.Reason = "native quota auto-resume is stale; explicit user confirmation required"
	case "quota_auto_resume_disabled":
		s.Reason = "native quota auto-resume disabled; guard ownership not inferred"
	}
	t.Log(now, "native_resume", s.ID, event.NotificationType)
	return s
}

// hostedNotification coordinates with Claude's own auto-resume for hosted
// sessions: a fired resume wins; a stale or disabled one hands over to the guard.
func hostedNotification(t *store.Tx, s domain.Session, event domain.HookEvent, now time.Time) domain.Session {
	s.UpdatedAt = event.ObservedAt
	switch event.NotificationType {
	case "quota_auto_resume_fired":
		invalidate(t, s.ID, "native quota auto-resume fired", now)
		s.Generation++
		t.HookBarriers[s.ID] = event.ObservedAt
		s.NativeResume = "fired"
		s.Error = ""
		recordResume(t, s.ID, event.ObservedAt)
		markRunning(t, s.ID, event.ObservedAt)
		s.State = domain.Running
		s.Reason = "native quota auto-resume fired; external resume invalidated"
	case "quota_auto_resume_stale", "quota_auto_resume_disabled":
		s.NativeResume = "stale"
		if event.NotificationType == "quota_auto_resume_disabled" {
			s.NativeResume = "disabled"
		}
		queued := false
		for _, j := range t.Jobs {
			if j.SessionID == s.ID && j.Generation == s.Generation && j.Kind == "" && (j.State == domain.Cooldown || j.State == domain.Ready) {
				queued = true
			}
		}
		if !queued && !busy(s.State) {
			s.State = domain.Cooldown
			enqueue(t, s, now)
		}
		s.Reason = "原生自动继续不可用，由 LimitBreak 接管"
	}
	t.Log(now, "native_resume", s.ID, event.NotificationType)
	return s
}

// Aging is uncapped: any waiting job eventually outranks newly arriving
// high-priority jobs. Equal effective priorities preserve FIFO then stable ID.
func agedPriority(t *store.Tx, job domain.Job, now time.Time) int64 {
	priority := sessionPriority(t.Sessions[job.SessionID])
	age := now.Sub(job.CreatedAt)
	if age < 0 {
		age = 0
	}
	interval := time.Duration(config.EffectivePolicy(*t.Policy).QueueAgingSeconds) * time.Second
	return int64(priority) + int64(age/interval)
}

// occupiesRunningBudget counts only turns this guard is responsible for: legacy
// guard bindings and turns it continued itself. Sessions the user is driving
// never hold back a hosted continue; a user prompt drops the guard's claim.
func occupiesRunningBudget(t *store.Tx, s domain.Session) bool {
	if s.State == domain.Dispatching || s.State == domain.Verifying {
		return false
	}
	if !legacyGuard(s) && t.RunningAccounts[s.ID] == "" {
		return false
	}
	if _, ok := t.RunningSince[s.ID]; ok {
		return true
	}
	return s.State == domain.Running
}
