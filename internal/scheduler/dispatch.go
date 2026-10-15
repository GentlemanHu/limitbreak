package scheduler

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func bindingProblem(s domain.Session) string {
	b := s.Binding
	if b == nil {
		return "no explicit binding"
	}
	if err := domain.ValidateResumeFields(*b); err != nil {
		return "invalid resume binding: " + err.Error()
	}
	if b.ResumeOwner != "guard" {
		return "resume ownership is native, manual or unknown; monitoring only"
	}
	if !b.NativeAutoResumeDisabledAcknowledged {
		return "native auto-resume disablement has not been explicitly acknowledged"
	}
	if b.Kind != "code" || s.Kind != "code" {
		return "only code sessions can auto-resume"
	}
	if !b.AutoResume {
		return "auto-resume is disabled"
	}
	if !b.AcknowledgeSideEffects {
		return "side effects have not been acknowledged"
	}
	if b.Provider == "" || b.Model == "" || len(b.AccountIDs) == 0 {
		return "binding has no explicit resource pool"
	}
	if s.Model != "" && s.Model != b.Model {
		return "session model differs from binding"
	}
	if b.TargetTitle == "" && b.TargetIdentifier == "" && b.DeepLink == "" {
		return "desktop target is missing"
	}
	return ""
}
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// An expired reset timestamp is not proof of quota recovery. Only fresh allowed
// evidence can authorize sending; limited.AvailableAt is solely a wake-up hint.
func accountReady(a domain.Account, b domain.Binding, p domain.Policy, now time.Time) bool {
	if len(b.ResumeResources) > 0 {
		return scopedAccountReady(a, b, p, now)
	}
	if a.Disabled || a.QuotaError != "" || a.State != "allowed" || (b.Provider != "" && a.Provider != b.Provider) || !contains(b.AccountIDs, a.ID) || !domain.ModelIn(a.Models, b.Model) {
		return false
	}
	if a.CheckedAt.IsZero() || a.CheckedAt.After(now) || now.Sub(a.CheckedAt) > time.Duration(p.FreshnessSeconds)*time.Second {
		return false
	}
	if len(a.Windows) == 0 {
		return false
	}
	for _, g := range a.Groups {
		// A group scoped to another model family or to an operation such as
		// code review does not limit this session.
		if domain.GroupApplies(g, b.Model) && !groupReady(g, p, now) {
			return false
		}
	}
	for _, w := range a.Windows {
		if w.Blocked || w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent >= 100 {
			return false
		}
	}
	return true
}

// candidate selects a ready account from the plan's pool. The returned hint is
// the earliest observed reset time, used to give native auto-resume priority.
func candidate(t *store.Tx, s domain.Session, b domain.Binding, j domain.Job, now time.Time) (string, time.Time, string, *time.Time) {
	p := config.EffectivePolicy(*t.Policy)
	next := now.Add(time.Duration(p.UnknownProbeSeconds) * time.Second)
	reason := "unknown or stale quota evidence"
	auto := b.ResumeOwner == "auto"
	var eligible, unverified []domain.Account
	var earliest *time.Time
	fallback := p.ResetFallback != domain.ResetFallbackOff
	safety := time.Duration(p.SafetySeconds) * time.Second
	for _, a := range t.Accounts {
		if !auto && contains(b.AccountIDs, a.ID) && !boundSourceMatches(t, s.ID, a) {
			reason = "account source identity changed or unresolved; explicit re-binding required"
			continue
		}
		if !domain.SourceConnected(a, t.Capabilities) || a.Disabled || !contains(b.AccountIDs, a.ID) || (b.Provider != "" && a.Provider != b.Provider) || !domain.ModelIn(a.Models, b.Model) {
			continue
		}
		ok, viaReset := quotaEligible(a, b, p, t.QuotaFailures[s.ID], now)
		if ok && !viaReset {
			eligible = append(eligible, a)
			continue
		}
		if ok {
			unverified = append(unverified, a)
			continue
		}
		hint := accountResetHint(a, b)
		// The quota query itself can be throttled (Anthropic's usage endpoint
		// answers 429 under load). Then the recorded block and its reset are
		// the only evidence, so the wait wakes at that reset.
		if hint == nil && fallback && a.State != "allowed" && domain.QuotaQueryThrottled(a.QuotaError) {
			hint, _ = domain.ResetPassed(a, b.Model, t.QuotaFailures[s.ID], safety, now)
		}
		if hint != nil {
			if earliest == nil || hint.Before(*earliest) {
				h := *hint
				earliest = &h
			}
			at := hint.Add(time.Duration(p.SafetySeconds) * time.Second)
			if !at.After(now) {
				at = now.Add(30 * time.Second)
			}
			if reason != "waiting for quota reset" || at.Before(next) {
				next = at
			}
			reason = "waiting for quota reset"
		}
	}
	ready := "quota ready"
	if len(eligible) == 0 && len(unverified) > 0 {
		eligible, ready = unverified, reasonResetFallback
	}
	if len(eligible) > 0 && auto && j.ResetHint != nil && s.NativeResume != "stale" && s.NativeResume != "disabled" {
		// Claude's own auto-resume fires at the reset it observed; yield to it
		// for a grace period so the session never receives two continues.
		// Quota that is back well before that reset (the provider reset limits
		// early) is not something Claude will act on before then, so no wait.
		grace := time.Duration(p.NativeGraceSeconds) * time.Second
		if until := j.ResetHint.Add(grace); now.Before(until) && !now.Before(j.ResetHint.Add(-grace)) {
			return "", until, reasonNativeGrace, earliest
		}
	}
	// Stable ordering avoids guessing based on email, plan or model aliases.
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })
	for _, a := range eligible {
		if accountUse(t, a.ID) < p.MaxPerAccount {
			return a.ID, now, ready, earliest
		}
	}
	if len(eligible) > 0 {
		return "", now.Add(time.Second), concurrencyReason(t, eligible, p.MaxPerAccount), earliest
	}
	return "", next, reason, earliest
}

// concurrencyReason names what holds the pool so the waiting session can say
// exactly which turn it waits for; the stable prefix keeps older readers working.
func concurrencyReason(t *store.Tx, eligible []domain.Account, limit int) string {
	seen := map[string]bool{}
	var holders []string
	for _, a := range eligible {
		for _, name := range accountHolders(t, a.ID) {
			if !seen[name] {
				seen[name] = true
				holders = append(holders, name)
			}
		}
	}
	sort.Strings(holders)
	if len(holders) == 0 {
		return "account concurrency limit"
	}
	return fmt.Sprintf("account concurrency limit: %d per account, held by %s", limit, strings.Join(holders, "、"))
}
func accountHolders(t *store.Tx, id string) []string {
	var names []string
	for _, s := range t.Sessions {
		if occupiesRunningBudget(t, s) {
			selected := t.RunningAccounts[s.ID]
			if selected == id || (selected == "" && legacyGuard(s) && contains(s.Binding.AccountIDs, id)) {
				names = append(names, sessionLabel(s))
			}
		}
	}
	for _, j := range t.Jobs {
		if (j.State == domain.Dispatching || j.State == domain.Verifying) && j.AccountID == id {
			names = append(names, sessionLabel(t.Sessions[j.SessionID]))
		}
	}
	return names
}
func sessionLabel(s domain.Session) string {
	name := strings.TrimSpace(strings.ReplaceAll(s.Title, "\n", " "))
	if name == "" {
		name = filepath.Base(s.Cwd)
	}
	if name == "" || name == "." || name == "/" {
		name = s.ID
		if len(name) > 8 {
			name = name[:8]
		}
	}
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40]) + "…"
	}
	return "「" + name + "」"
}
func accountUse(t *store.Tx, id string) int {
	n := 0
	for _, s := range t.Sessions {
		if occupiesRunningBudget(t, s) {
			selected := t.RunningAccounts[s.ID]
			if selected == id || (selected == "" && legacyGuard(s) && contains(s.Binding.AccountIDs, id)) {
				n++
			}
		}
	}
	for _, j := range t.Jobs {
		if (j.State == domain.Dispatching || j.State == domain.Verifying) && j.AccountID == id {
			n++
		}
	}
	return n
}
func totalUse(t *store.Tx) int {
	n := 0
	for _, s := range t.Sessions {
		if occupiesRunningBudget(t, s) {
			n++
		}
	}
	for _, j := range t.Jobs {
		if j.State == domain.Dispatching || j.State == domain.Verifying {
			n++
		}
	}
	return n
}

func (e *Engine) scan(ctx context.Context, workers *sync.WaitGroup) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var claimed *domain.Job
	var binding domain.Binding
	var policy domain.Policy
	now := e.now()
	err := e.store.Update(ctx, func(t *store.Tx) error {
		if t.Policy == nil {
			return nil
		}
		policy = config.EffectivePolicy(*t.Policy)
		markStaleRunning(t, now)
		releaseEndedRunning(t, e.lookup, now)
		expireLeases(t, now, e.cancelActive)
		if e.bridge == nil || (t.DesktopOwner != "" && t.DesktopUntil.After(now)) {
			return nil
		}
		// Explicit user test sends bypass quota gates but never the delivery lease.
		if j, b, ok := claimTest(t, policy, now); ok {
			job, leased := e.claim(t, j, b, "", policy, now)
			binding, claimed = leased, &job
			return nil
		}
		if policy.Paused || !t.Capabilities.ProxyConnected || e.quota == nil {
			return nil
		}
		if used := totalUse(t); used >= policy.MaxRunning {
			noteConcurrencyWait(t, used, policy.MaxRunning, now)
			return nil
		}
		if now.Before(t.LastSend.Add(time.Duration(policy.MinSendIntervalSeconds) * time.Second)) {
			return nil
		}
		jobs := make([]domain.Job, 0, len(t.Jobs))
		for _, j := range t.Jobs {
			if j.Kind == "" && (j.State == domain.Cooldown || j.State == domain.Ready) {
				jobs = append(jobs, j)
			}
		}
		sort.Slice(jobs, func(i, j int) bool {
			pi, pj := agedPriority(t, jobs[i], now), agedPriority(t, jobs[j], now)
			if pi != pj {
				return pi > pj
			}
			if !jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
				return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
			}
			return jobs[i].ID < jobs[j].ID
		})
		for _, j := range jobs {
			s, ok := t.Sessions[j.SessionID]
			if !ok || s.Generation != j.Generation || (s.State != domain.Cooldown && s.State != domain.Ready) {
				j.State = domain.Cancelled
				j.Reason = "obsolete generation"
				j.UpdatedAt = now
				t.Jobs[j.ID] = j
				continue
			}
			b, problem := plan(t.Accounts, s, policy)
			if problem != "" {
				j.Reason = problem
				t.Jobs[j.ID] = j
				s.Reason = problem
				t.Sessions[s.ID] = s
				continue
			}
			if resumeLimitReached(t, s.ID, now) {
				j.State, j.Reason, j.UpdatedAt = domain.Attention, "automatic resume hourly circuit breaker; manual confirmation required", now
				t.Jobs[j.ID] = j
				s.State, s.Reason, s.UpdatedAt = domain.Attention, j.Reason, now
				t.Sessions[s.ID] = s
				t.Log(now, "resume_circuit_breaker", s.ID, j.Reason)
				continue
			}
			if strings.HasPrefix(j.Reason, "waiting for desktop:") && j.NotBefore.After(now) {
				continue
			}
			// Re-evaluate evidence on every timer scan, including after a read-only refresh.
			// NotBefore remains a hint, never permission to send stale data.
			account, next, reason, hint := candidate(t, s, b, j, now)
			if b.ResumeOwner == "auto" && hint != nil && (j.ResetHint == nil || hint.After(*j.ResetHint)) {
				j.ResetHint = hint
			}
			if account == "" {
				if reason == "unknown or stale quota evidence" && j.Reason == reason && j.NotBefore.After(now) {
					next = j.NotBefore
				}
				if j.State != domain.Cooldown || !j.NotBefore.Equal(next) || j.Reason != reason {
					j.UpdatedAt = now
				}
				j.State = domain.Cooldown
				j.NotBefore = next
				j.Reason = reason
				t.Jobs[j.ID] = j
				s.State = domain.Cooldown
				s.Reason = reason
				t.Sessions[s.ID] = s
				continue
			}
			j.MessageDigest = digestText(b.ContinueMessage)
			if reason == reasonResetFallback {
				t.Log(now, "dispatch", s.ID, reasonResetFallback+"（账号 "+account+"）")
			}
			job, leased := e.claim(t, j, b, account, policy, now)
			binding, claimed = leased, &job
			break
		}
		return nil
	})
	if err != nil || claimed == nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(policy.VerifySeconds)*time.Second)
	e.active[claimed.SessionID] = activeCall{generation: claimed.Generation, cancel: cancel}
	workers.Add(1)
	go func() { defer workers.Done(); defer cancel(); e.dispatch(callCtx, *claimed, binding, policy) }()
	return nil
}

// expireLeases turns unconfirmed claims into attention; they are never resent.
func expireLeases(t *store.Tx, now time.Time, cancel func(string)) {
	for id, j := range t.Jobs {
		if (j.State == domain.Dispatching || j.State == domain.Verifying) && (j.LeaseUntil == nil || !j.LeaseUntil.After(now)) {
			j.State = domain.Attention
			j.Reason = "dispatch confirmation timed out; do not resend"
			j.UpdatedAt = now
			j.LeaseUntil = nil
			t.Jobs[id] = j
			s := t.Sessions[j.SessionID]
			if s.Generation == j.Generation {
				s.State = domain.Attention
				s.Reason = j.Reason
				s.UpdatedAt = now
				t.Sessions[s.ID] = s
			}
			cancel(j.SessionID)
			t.Log(now, "attention", j.SessionID, j.Reason)
		}
	}
}

// claim persists the lease before any side effect. A crash in this phase is
// conservatively unconfirmed/attention on lease expiry; it is never replayed.
func (e *Engine) claim(t *store.Tx, j domain.Job, b domain.Binding, account string, policy domain.Policy, now time.Time) (domain.Job, domain.Binding) {
	until := now.Add(time.Duration(policy.VerifySeconds) * time.Second)
	j.State = domain.Dispatching
	j.Delivery = ""
	j.AccountID = account
	j.LeaseUntil = &until
	j.Attempts++
	j.Reason = "atomic desktop and resource lease acquired"
	j.UpdatedAt = now
	if j.MessageDigest == "" {
		j.MessageDigest = digestText(b.ContinueMessage)
	}
	t.Jobs[j.ID] = j
	s := t.Sessions[j.SessionID]
	s.State = domain.Dispatching
	s.Reason = j.Reason
	s.UpdatedAt = now
	t.Sessions[s.ID] = s
	t.DispatchStarted[j.ID] = now
	t.DesktopOwner = e.owner + "/" + j.ID
	t.DesktopUntil = until
	if account != "" {
		b.AccountIDs = []string{account}
	}
	b.ExecutionGeneration = j.Generation
	b.ExecutionLease = e.owner + "/" + j.ID
	b.ExecutionUserIdleSeconds = policy.UserIdleSeconds
	b.ExecutionDirect = policy.DirectDelivery != domain.DirectOff
	b.ExecutionRestoreFocus = policy.RestoreFocus != domain.DirectOff
	b.ExecutionQuietSeconds = 0
	if policy.ClaudeQuiet != domain.DirectOff {
		b.ExecutionQuietSeconds = policy.ClaudeQuietSeconds
	}
	if j.Kind == "test" {
		t.Log(now, "test_send", s.ID, "manual test send leased")
	} else {
		t.Log(now, "dispatch", s.ID, "leased account "+account)
	}
	return j, b
}

// Only known, pre-Resume environmental failures are safe to probe again.
func retryablePreflight(r domain.BridgeResult) bool {
	if r.Submitted {
		return false
	}
	code := strings.ToLower(r.Code)
	if strings.Contains(code, "ambig") || strings.Contains(code, "identity") || strings.Contains(code, "target") {
		return false
	}
	if r.ScreenLocked {
		return true
	}
	if !r.AccessibilityGranted && (r.OK || code == "" || strings.Contains(code, "accessib") || code == "ax_denied") {
		return true
	}
	switch code {
	case "bridge_missing", "bridge_not_found", "bridge_unavailable", "user_active", "idle_state_unverified", "claude_in_use",
		"terminal_session_open", "terminal_not_foreground", "terminal_busy", "session_busy":
		return true
	}
	return false
}

// notSent lists executor codes raised strictly before any input was written.
// Only these may return a claimed job to the queue after Resume.
func notSent(r domain.BridgeResult) bool {
	if r.Submitted {
		return false
	}
	switch r.Code {
	case "user_active", "claude_in_use", "screen_locked", "screen_inactive", "console_inactive", "lock_state_unverified", "idle_state_unverified", "accessibility_denied", "bridge_unavailable",
		"terminal_session_open", "terminal_not_foreground", "terminal_busy", "session_busy":
		return true
	}
	return false
}

func desktopExecutor(b domain.Binding) bool {
	return b.Executor == "" || b.Executor == domain.ExecutorDesktop
}

// stillClaimed re-validates a claim against the latest persisted state.
func stillClaimed(t *store.Tx, job domain.Job, binding domain.Binding) bool {
	j := t.Jobs[job.ID]
	s := t.Sessions[job.SessionID]
	if s.Generation != job.Generation || j.State != domain.Dispatching || t.Policy == nil || (job.Kind != "test" && t.Policy.Paused) {
		return false
	}
	current, problem := jobBinding(t.Accounts, s, j, config.EffectivePolicy(*t.Policy))
	return problem == "" && sameExecution(binding, current, j)
}

func (e *Engine) preflight(ctx context.Context, b domain.Binding) domain.BridgeResult {
	if p, ok := e.bridge.(interface {
		Preflight(context.Context, domain.Binding) domain.BridgeResult
	}); ok {
		return p.Preflight(ctx, b)
	}
	return e.bridge.Probe(ctx)
}

func (e *Engine) dispatch(ctx context.Context, job domain.Job, binding domain.Binding, policy domain.Policy) {
	ctx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	test := job.Kind == "test"
	// Keep the shared desktop lease alive while a bridge subprocess is still alive,
	// even when it ignores cancellation. No second process may interact concurrently.
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-tick.C:
				c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				stale := false
				err := e.store.Update(c, func(t *store.Tx) error {
					if t.DesktopOwner == e.owner+"/"+job.ID {
						t.DesktopUntil = e.now().Add(time.Duration(policy.VerifySeconds) * time.Second)
					} else {
						stale = true
					}
					if !stillClaimed(t, job, binding) {
						stale = true
					}
					return nil
				})
				cancel()
				if stale || err != nil {
					cancelCall()
				}
			}
		}
	}()
	defer func() { stopHeartbeat(); <-heartbeatDone }()
	result := domain.BridgeResult{}
	var dispatchErr error
	var reserved time.Time
	waitPreflight := false
	resumed := false
	probe := e.preflight(ctx, binding)
	probedAt := e.now()
	// The direct channel never touches the screen, so neither Accessibility nor
	// the lock state gates it.
	direct := probe.Channel == domain.ChannelDirect
	if !probe.OK || (!direct && (!probe.AccessibilityGranted || probe.ScreenLocked)) {
		result = probe
		dispatchErr = fmt.Errorf("bridge preflight: %s", probe.Message)
		waitPreflight = ctx.Err() == nil && retryablePreflight(probe)
	} else {
		// Cancellation and generation are checked after preflight and directly before Resume.
		e.mu.Lock()
		dispatchErr = e.store.Update(ctx, func(t *store.Tx) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !stillClaimed(t, job, binding) {
				return fmt.Errorf("dispatch cancelled or superseded")
			}
			if t.DesktopOwner != e.owner+"/"+job.ID {
				return fmt.Errorf("desktop lease lost")
			}
			s := t.Sessions[job.SessionID]
			if !test {
				var account domain.Account
				for _, a := range t.Accounts {
					if a.ID == job.AccountID {
						account = a
						break
					}
				}
				sourceOK := domain.SourceConnected(account, t.Capabilities) && (binding.ResumeOwner == "auto" || boundSourceMatches(t, s.ID, account))
				if !sourceOK {
					return fmt.Errorf("quota evidence is no longer eligible")
				}
				if ok, _ := quotaEligible(account, binding, config.EffectivePolicy(*t.Policy), t.QuotaFailures[s.ID], e.now()); !ok {
					return fmt.Errorf("quota evidence is no longer eligible")
				}
				if resumeLimitReached(t, s.ID, e.now()) {
					return fmt.Errorf("automatic resume hourly circuit breaker")
				}
				// Reserve before the side effect; an uncertain send still consumes a
				// resume slot and cannot silently circumvent the hourly breaker.
				reserved = e.now()
				recordResume(t, s.ID, reserved)
			}
			t.LastSend = e.now()
			return nil
		})
		e.mu.Unlock()
		if dispatchErr == nil && ctx.Err() == nil {
			resumed = true
			result, dispatchErr = e.bridge.Resume(ctx, binding, binding.ContinueMessage)
			waitPreflight = ctx.Err() == nil && notSent(result)
		} else if dispatchErr == nil {
			dispatchErr = ctx.Err()
		}
	}
	// Persist outcome even when the operation context was cancelled. Never set RUNNING
	// here: a successful return still requires a fresh session hook.
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.store.Update(saveCtx, func(t *store.Tx) error {
		if t.DesktopOwner == e.owner+"/"+job.ID {
			t.DesktopOwner = ""
			t.DesktopUntil = time.Time{}
		}
		// This was an actual desktop observation even if a newer lifecycle event
		// superseded the job while the bridge was returning.
		if desktopExecutor(binding) && !direct {
			applyDesktopResult(t, probe, probedAt)
		}
		j, ok := t.Jobs[job.ID]
		s := t.Sessions[job.SessionID]
		if !ok || s.Generation != job.Generation || j.State != domain.Dispatching {
			return nil
		}
		now := e.now()
		failed := result
		if failed.Code == "" {
			failed = probe
		}
		switch {
		case waitPreflight && test && j.Attempts < 10:
			j.State = domain.Ready
			j.LeaseUntil = nil
			j.NotBefore = now.Add(5 * time.Second)
			j.Reason = "waiting for desktop: " + failed.Code + " " + failed.Message
			delete(t.DispatchStarted, j.ID)
		case waitPreflight && !test:
			j.State = domain.Cooldown
			j.LeaseUntil = nil
			j.AccountID = ""
			j.Attempts--
			j.NotBefore = now.Add(30 * time.Second)
			j.Reason = "waiting for desktop: " + failed.Code + " " + failed.Message
			delete(t.DispatchStarted, j.ID)
			if !reserved.IsZero() {
				unrecordResume(t, s.ID, reserved)
			}
		case dispatchErr != nil || ctx.Err() != nil || !result.OK || !result.Submitted:
			j.State = domain.Attention
			j.LeaseUntil = nil
			j.Reason = "submission failed or uncertain; manual confirmation required"
			if unsent := !failed.Submitted && failed.Code != "" && failed.Code != "draft_retained" && failed.Code != "submitted_uncertain"; unsent && (!resumed || result.Code != "") {
				// The executor refused before writing anything; say so plainly.
				j.Reason = "未发送（" + failed.Code + "）：" + failed.Message
			} else if dispatchErr != nil {
				j.Reason += ": " + dispatchErr.Error()
			} else if result.Code != "" {
				j.Reason += ": " + result.Code
			}
		default:
			j.State = domain.Verifying
			j.Reason = "submitted; waiting for a fresh hook"
			j.Delivery = domain.DeliveryWindow
			switch {
			case result.Code == "direct_delivered" && result.Woken:
				j.Reason = "submitted via direct channel after waking the session; waiting for a fresh hook"
				j.Delivery = domain.DeliveryWokenDirect
			case result.Code == "direct_sent_unconfirmed" && result.Woken:
				j.Reason = "submitted via direct channel after waking the session, turn not started yet"
				j.Delivery = domain.DeliveryWokenDirect
			case result.Code == "direct_delivered":
				j.Reason = "submitted via direct channel; waiting for a fresh hook"
				j.Delivery = domain.DeliveryDirect
			case result.Code == "direct_sent_unconfirmed":
				j.Reason = "submitted via direct channel, turn not started yet"
				j.Delivery = domain.DeliveryDirect
			case result.Woken:
				j.Reason = "submitted via the Claude window after the session process did not start; waiting for a fresh hook"
				j.Delivery = domain.DeliveryWindowFallback
			}
			until := now.Add(time.Duration(policy.VerifySeconds) * time.Second)
			j.LeaseUntil = &until
		}
		j.UpdatedAt = now
		t.Jobs[j.ID] = j
		s.State = j.State
		s.Reason = j.Reason
		s.UpdatedAt = now
		t.Sessions[s.ID] = s
		t.Log(now, "bridge_result", s.ID, j.Reason)
		return nil
	})
	if err != nil {
		_ = e.recordError(saveCtx, "dispatch_persistence", err)
	}
	if a, ok := e.active[job.SessionID]; ok && a.generation == job.Generation {
		delete(e.active, job.SessionID)
	}
	e.signal()
}
