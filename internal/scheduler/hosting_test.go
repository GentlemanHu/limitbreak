package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

const testHostSession = "local_0a1b2c3d-1111-4222-8333-444455556666"

func desktopOrigin(at time.Time) *domain.SessionOrigin {
	return &domain.SessionOrigin{App: domain.AppClaudeDesktop, Entrypoint: "claude-desktop-3p", BundleID: domain.DesktopBundleID, HostSessionID: testHostSession, ObservedAt: at}
}

func hostedEngine(t *testing.T, bridge Bridge, consent bool) (*Engine, *fixtureQuota, *manualClock) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := &manualClock{at: time.Now().UTC().Truncate(time.Second)}
	p := policy()
	if consent {
		at := clock.at.Add(-time.Hour)
		p.AutomationConsentAt = &at
	}
	q := &fixtureQuota{}
	return NewWithClock(db, q, bridge, p, clock), q, clock
}

func hostedFixture(t *testing.T, consent bool) (*Engine, *fixtureQuota, *fixtureBridge, *manualClock) {
	t.Helper()
	b := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}
	e, q, clock := hostedEngine(t, b, consent)
	return e, q, b, clock
}

// hostedEvent is a hook event carrying the desktop origin and a Code kind.
func hostedEvent(t *testing.T, e *Engine, id, name string, at time.Time) {
	t.Helper()
	v := domain.HookEvent{EventID: newID(), SessionID: id, HookEventName: name, ObservedAt: at, Kind: "code", Model: "opus[1m]", Origin: desktopOrigin(at)}
	if name == "StopFailure" {
		v.Error = "rate_limit_error"
	}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func notify(t *testing.T, e *Engine, id, kind string, at time.Time) {
	t.Helper()
	v := domain.HookEvent{EventID: newID(), SessionID: id, HookEventName: "Notification", NotificationType: kind, ObservedAt: at}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func freshAccount(id string, at time.Time) domain.Account {
	a := allowed(id)
	a.CheckedAt = at
	return a
}

func jobsOf(t *testing.T, e *Engine, id string) []domain.Job {
	t.Helper()
	snap, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.Job
	for _, j := range snap.Jobs {
		if j.SessionID == id {
			out = append(out, j)
		}
	}
	return out
}

func TestResolvePoolMatchesSessionModel(t *testing.T) {
	accounts := []domain.Account{
		{ID: "c2", Provider: "claude", Models: []string{"claude-sonnet-4-5"}},
		{ID: "c1", Provider: "claude", Models: []string{"claude-opus-4-8"}},
		{ID: "x1", Provider: "codex", Models: []string{"gpt-5"}},
		{ID: "c3", Provider: "claude", Models: []string{"claude-opus-4-8"}, Disabled: true},
	}
	cases := []struct {
		name     string
		session  domain.Session
		aliases  map[string]string
		ids      []string
		basis    string
		model    string
		provider string
		inferred bool
	}{
		{"exact", domain.Session{Model: "Claude-Opus-4-8[1m]"}, nil, []string{"c1"}, "exact", "claude-opus-4-8", "claude", false},
		{"alias", domain.Session{Model: "opus"}, map[string]string{"opus": "claude-sonnet-4-5"}, []string{"c2"}, "alias", "claude-sonnet-4-5", "claude", false},
		{"prefix", domain.Session{Model: "claude-opus-4-8-20260101"}, nil, []string{"c1"}, "prefix", "claude-opus-4-8", "claude", true},
		{"provider", domain.Session{Model: "claude-haiku-9"}, nil, []string{"c1", "c2"}, "provider", "", "claude", true},
		{"override", domain.Session{Model: "opus", PoolOverride: []string{"x1", "c3"}}, nil, []string{"x1"}, "manual", "", "codex", false},
		{"none", domain.Session{Model: "llama-3"}, nil, []string{}, "none", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := policy()
			p.ModelAliases = c.aliases
			v := resolvePool(accounts, c.session, p)
			if !reflect.DeepEqual(v.AccountIDs, c.ids) || v.Basis != c.basis || v.Model != c.model || v.Provider != c.provider || v.Inferred != c.inferred {
				t.Fatalf("unexpected pool %+v", v)
			}
		})
	}
}

func TestHostedDispatchRequiresConsentAndUsesDeepLink(t *testing.T) {
	e, q, b, clock := hostedFixture(t, false)
	ctx := context.Background()
	hostedEvent(t, e, "s", "StopFailure", clock.at)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	s := state(t, e, "s")
	if b.count() != 0 || s.Host == nil || s.Host.Ready || s.Host.Executor != domain.ExecutorDesktop || s.Pool == nil || !reflect.DeepEqual(s.Pool.AccountIDs, []string{"a"}) {
		t.Fatalf("sent without consent or wrong explanation: host=%+v pool=%+v", s.Host, s.Pool)
	}
	snap, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := e.PatchPolicy(ctx, snap.PolicyDigest, map[string]json.RawMessage{"automation_consent": json.RawMessage("true")})
	if err != nil {
		t.Fatal(err)
	}
	if updated.AutomationConsentAt == nil || !updated.AutomationConsentAt.Equal(clock.at) {
		t.Fatal("consent must be stamped with server time")
	}
	scan(t, e)
	if b.count() != 1 {
		t.Fatalf("hosted session not resumed after consent, calls=%d", b.count())
	}
	got := b.bindings[0]
	if got.Executor != domain.ExecutorDesktop || got.DeepLink != domain.DesktopDeepLink(testHostSession) || got.ResumeOwner != "auto" || b.messages[0] != policy().ContinueMessage {
		t.Fatalf("wrong delivery %+v", got)
	}
	if state(t, e, "s").State != domain.Verifying {
		t.Fatal("submission must wait for a hook")
	}
	hostedEvent(t, e, "s", "UserPromptSubmit", clock.at.Add(time.Second))
	if state(t, e, "s").State != domain.Running {
		t.Fatal("prompt hook did not confirm hosted resume")
	}
	confirmed := false
	for _, j := range jobsOf(t, e, "s") {
		confirmed = confirmed || (j.State == domain.Completed && j.AccountID == "a")
	}
	if !confirmed {
		t.Fatal("hosted job not confirmed")
	}
}

func TestHostedNativeGraceYieldsToClaudeAutoResume(t *testing.T) {
	for _, kind := range []string{"none", "quota_auto_resume_fired", "quota_auto_resume_stale"} {
		t.Run(kind, func(t *testing.T) {
			e, q, b, clock := hostedFixture(t, true)
			start := clock.at
			hostedEvent(t, e, "s", "StopFailure", start)
			clock.at = start.Add(time.Second)
			limited := freshAccount("a", clock.at)
			limited.State = "limited"
			reset := start.Add(10 * time.Minute)
			limited.AvailableAt = &reset
			limited.Windows[0].Blocked = true
			refresh(t, e, q, limited)
			scan(t, e)
			jobs := jobsOf(t, e, "s")
			if b.count() != 0 || len(jobs) != 1 || jobs[0].ResetHint == nil {
				t.Fatalf("reset hint not recorded: %+v", jobs)
			}
			clock.at = reset.Add(5 * time.Second)
			refresh(t, e, q, freshAccount("a", clock.at))
			if kind != "none" {
				notify(t, e, "s", kind, clock.at)
			}
			scan(t, e)
			switch kind {
			case "quota_auto_resume_stale":
				if b.count() != 1 {
					t.Fatal("stale native resume must hand over immediately")
				}
				return
			case "none":
				if b.count() != 0 || state(t, e, "s").Reason != reasonNativeGrace {
					t.Fatalf("did not yield to native resume: %q", state(t, e, "s").Reason)
				}
			case "quota_auto_resume_fired":
				if b.count() != 0 || state(t, e, "s").State != domain.Running {
					t.Fatal("native resume not recorded")
				}
			}
			clock.at = reset.Add(91 * time.Second)
			refresh(t, e, q, freshAccount("a", clock.at))
			scan(t, e)
			want := 1
			if kind == "quota_auto_resume_fired" {
				want = 0
			}
			if b.count() != want {
				t.Fatalf("after grace calls=%d want %d", b.count(), want)
			}
		})
	}
}

// A provider can reset limits days early. Claude's own auto-resume waits for
// the reset it observed, so fresh quota long before it must not wait too.
func TestHostedEarlyResetSkipsNativeGrace(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	start := clock.at
	hostedEvent(t, e, "s", "StopFailure", start)
	clock.at = start.Add(time.Second)
	limited := freshAccount("a", clock.at)
	limited.State = "limited"
	reset := start.Add(96 * time.Hour)
	limited.AvailableAt = &reset
	limited.Windows[0].Blocked = true
	refresh(t, e, q, limited)
	scan(t, e)
	if jobs := jobsOf(t, e, "s"); b.count() != 0 || len(jobs) != 1 || jobs[0].ResetHint == nil {
		t.Fatalf("reset hint not recorded: %+v", jobs)
	}
	clock.at = start.Add(20 * time.Minute)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 1 || state(t, e, "s").State != domain.Verifying {
		t.Fatalf("early reset waited for native grace: calls=%d reason=%q", b.count(), state(t, e, "s").Reason)
	}
}

func TestHostedUserActiveRequeuesWithoutConsumingBreaker(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	hostedEvent(t, e, "s", "StopFailure", clock.at)
	b.result = domain.BridgeResult{Code: "user_active", Message: "user typing"}
	for i := 0; i < 4; i++ {
		clock.at = clock.at.Add(31 * time.Second)
		refresh(t, e, q, freshAccount("a", clock.at))
		scan(t, e)
		if b.count() != i+1 || state(t, e, "s").State != domain.Cooldown {
			t.Fatalf("attempt %d: calls=%d state=%s reason=%s", i, b.count(), state(t, e, "s").State, state(t, e, "s").Reason)
		}
	}
	b.result = domain.BridgeResult{OK: true, Submitted: true}
	clock.at = clock.at.Add(31 * time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 5 || state(t, e, "s").State != domain.Verifying {
		t.Fatalf("unsent attempts consumed the breaker: state=%s", state(t, e, "s").Reason)
	}
}

func TestHostedModelChangeKeepsQueuedJob(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	hostedEvent(t, e, "s", "StopFailure", clock.at)
	v := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "ModelChange", NewModel: "sonnet", ObservedAt: clock.at.Add(time.Second)}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	s := state(t, e, "s")
	if s.State != domain.Cooldown || s.Model != "sonnet" || len(jobsOf(t, e, "s")) != 1 || jobsOf(t, e, "s")[0].State != domain.Cooldown {
		t.Fatalf("hosted model change dropped the queue: %+v", s)
	}
}

func TestTestSendDeliversCustomTextAndNeedsHook(t *testing.T) {
	e, _, b, clock := hostedFixture(t, false)
	ctx := context.Background()
	if _, err := e.TestSend(ctx, "missing", ""); err == nil {
		t.Fatal("unknown session accepted")
	}
	hostedEvent(t, e, "s", "Stop", clock.at)
	if _, err := e.TestSend(ctx, "s", "a\nb"); err == nil {
		t.Fatal("multi-line message accepted")
	}
	text := "继续测试"
	if _, err := e.UpdateSessionSettings(ctx, "s", domain.SessionSettings{ContinueMessage: &text}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TestSend(ctx, "s", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.TestSend(ctx, "s", ""); err == nil {
		t.Fatal("second concurrent test send accepted")
	}
	scan(t, e)
	if b.count() != 1 || b.messages[0] != text || b.bindings[0].DeepLink != domain.DesktopDeepLink(testHostSession) {
		t.Fatalf("test send delivered wrong text or target: %+v %v", b.bindings, b.messages)
	}
	if state(t, e, "s").State != domain.Verifying {
		t.Fatal("test send must wait for a hook")
	}
	if _, err := e.TestSend(ctx, "s", ""); err == nil {
		t.Fatal("test send accepted while verifying")
	}
	hostedEvent(t, e, "s", "UserPromptSubmit", clock.at.Add(time.Second))
	jobs := jobsOf(t, e, "s")
	if state(t, e, "s").State != domain.Running || len(jobs) != 1 || jobs[0].State != domain.Completed || jobs[0].Kind != "test" {
		t.Fatalf("test send not confirmed: %+v", jobs)
	}
}

func TestTestSendNeedsOriginAndWaitsWhileUserActive(t *testing.T) {
	e, _, b, clock := hostedFixture(t, false)
	ctx := context.Background()
	hook(t, e, "plain", "Stop", clock.at)
	if _, err := e.TestSend(ctx, "plain", ""); err == nil || err.Error() != reasonNoOrigin {
		t.Fatalf("session without origin accepted: %v", err)
	}
	hostedEvent(t, e, "s", "Stop", clock.at)
	b.result = domain.BridgeResult{Code: "user_active"}
	if _, err := e.TestSend(ctx, "s", ""); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	scan(t, e)
	if b.count() != 1 || state(t, e, "s").State != domain.Ready {
		t.Fatalf("user activity must defer, not fail: calls=%d state=%s", b.count(), state(t, e, "s").State)
	}
	b.result = domain.BridgeResult{OK: true, Submitted: true}
	clock.at = clock.at.Add(6 * time.Second)
	scan(t, e)
	if b.count() != 2 || state(t, e, "s").State != domain.Verifying {
		t.Fatal("deferred test send did not retry")
	}
}

func TestUpdateSessionSettingsValidatesAndApplies(t *testing.T) {
	e, q, _, clock := hostedFixture(t, true)
	ctx := context.Background()
	hostedEvent(t, e, "s", "Stop", clock.at)
	refresh(t, e, q, freshAccount("a", clock.at), freshAccount("b", clock.at))
	bad := "sometimes"
	if _, err := e.UpdateSessionSettings(ctx, "s", domain.SessionSettings{HostMode: &bad}); err == nil {
		t.Fatal("invalid host mode accepted")
	}
	badTarget := "terminal:1"
	if _, err := e.UpdateSessionSettings(ctx, "s", domain.SessionSettings{TargetOverride: &badTarget}); err == nil {
		t.Fatal("invalid target accepted")
	}
	target, pool, priority := "desktop", []string{"b"}, 7
	if _, err := e.UpdateSessionSettings(ctx, "s", domain.SessionSettings{TargetOverride: &target, PoolOverride: &pool, Priority: &priority}); err != nil {
		t.Fatal(err)
	}
	s := state(t, e, "s")
	if s.Host == nil || !s.Host.Ready || s.Host.Executor != domain.ExecutorDesktop || s.Host.Target == "" || s.Pool.Basis != "manual" || !reflect.DeepEqual(s.Pool.AccountIDs, pool) || s.Priority != priority {
		t.Fatalf("settings not applied: host=%+v pool=%+v", s.Host, s.Pool)
	}
	monitor := domain.HostMonitor
	if _, err := e.UpdateSessionSettings(ctx, "s", domain.SessionSettings{HostMode: &monitor}); err != nil {
		t.Fatal(err)
	}
	if s := state(t, e, "s"); s.Host.Ready || s.Host.Mode != domain.HostMonitor {
		t.Fatalf("monitor mode still hosted: %+v", s.Host)
	}
}

func TestPatchedPolicyAutomationConsent(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	p, err := patchedPolicy(policy(), map[string]json.RawMessage{"automation_consent": json.RawMessage("true")}, now)
	if err != nil || p.AutomationConsentAt == nil || !p.AutomationConsentAt.Equal(now) {
		t.Fatalf("consent not stamped: %v %v", p.AutomationConsentAt, err)
	}
	p, err = patchedPolicy(p, map[string]json.RawMessage{"automation_consent": json.RawMessage("false")}, now)
	if err != nil || p.AutomationConsentAt != nil {
		t.Fatal("consent not revoked")
	}
	if _, err := patchedPolicy(p, map[string]json.RawMessage{"automation_consent_at": json.RawMessage(`"2020-01-01T00:00:00Z"`)}, now); !errors.Is(err, store.ErrControlInvalid) {
		t.Fatal("client-supplied consent timestamp accepted")
	}
	if _, err := patchedPolicy(p, map[string]json.RawMessage{"model_aliases": json.RawMessage(`{"Opus":"claude-opus-4-8"}`)}, now); err == nil {
		t.Fatal("non-normalized alias key accepted")
	}
	p, err = patchedPolicy(p, map[string]json.RawMessage{"model_aliases": json.RawMessage(`{"opus":"claude-opus-4-8"}`), "default_host_mode": json.RawMessage(`"monitor"`)}, now)
	if err != nil || p.ModelAliases["opus"] != "claude-opus-4-8" || p.DefaultHostMode != domain.HostMonitor {
		t.Fatalf("alias or host mode not applied: %v", err)
	}
}

type locatingBridge struct {
	*fixtureBridge
	located []domain.Binding
}

func (l *locatingBridge) Locate(_ context.Context, b domain.Binding) domain.BridgeResult {
	l.located = append(l.located, b)
	return domain.BridgeResult{OK: true}
}

func TestLocateResolvesTargetWithoutTyping(t *testing.T) {
	lb := &locatingBridge{fixtureBridge: &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}}
	e, _, clock := hostedEngine(t, lb, false)
	ctx := context.Background()
	hostedEvent(t, e, "s", "Stop", clock.at)
	result, err := e.Locate(ctx, "s")
	if err != nil || !result.OK || len(lb.located) != 1 || lb.located[0].DeepLink != domain.DesktopDeepLink(testHostSession) || lb.count() != 0 {
		t.Fatalf("locate failed: %v %+v", err, lb.located)
	}
	hook(t, e, "plain", "Stop", clock.at)
	if _, err := e.Locate(ctx, "plain"); err == nil {
		t.Fatal("located a session without origin")
	}
}

func TestTerminalOriginWithoutCLIIdentityIsMonitorOnly(t *testing.T) {
	e, _, b, clock := hostedFixture(t, true)
	origin := &domain.SessionOrigin{App: "legacy-label", TermProgram: "iTerm.app", ClaudePID: 4242, Ancestors: []int{4242, 4000}, ObservedAt: clock.at}
	v := domain.HookEvent{EventID: newID(), SessionID: "term", HookEventName: "StopFailure", Error: "rate_limit_error", ObservedAt: clock.at, Kind: "code", Model: "opus", Origin: origin}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	s := state(t, e, "term")
	if s.Origin == nil || s.Origin.App != domain.AppTerminal {
		t.Fatalf("origin app not re-derived: %+v", s.Origin)
	}
	if s.Host == nil || s.Host.Ready || s.Host.Executor != "" || s.Host.Problem == "" {
		t.Fatalf("terminal session must not be routed to any executor: %+v", s.Host)
	}
	scan(t, e)
	if b.count() != 0 {
		t.Fatal("terminal session reached the desktop executor")
	}
}

// throttled is an account whose quota query answers 429: state unknown, the
// recorded 5h block still present with its reset time.
func throttled(id string, checked, reset time.Time) domain.Account {
	full := 100.0
	a := allowed(id)
	a.State = "unknown"
	a.QuotaError = "quota request returned HTTP 429"
	a.CheckedAt = checked
	a.Windows = []domain.Window{{Name: "observed anthropic:5h", Blocked: true, UsedPercent: &full, ResetAt: &reset}}
	return a
}

func TestHostedResetFallbackWhenQuotaQueryThrottled(t *testing.T) {
	setup := func(t *testing.T) (*Engine, *fixtureQuota, *fixtureBridge, *manualClock, time.Time) {
		e, q, b, clock := hostedFixture(t, true)
		hostedEvent(t, e, "s", "StopFailure", clock.at)
		reset := clock.at.Add(time.Hour)
		clock.at = clock.at.Add(time.Second)
		refresh(t, e, q, throttled("a", clock.at, reset))
		return e, q, b, clock, reset
	}
	t.Run("waits then tries once", func(t *testing.T) {
		e, q, b, clock, reset := setup(t)
		scan(t, e)
		if b.count() != 0 {
			t.Fatal("sent before the recorded reset passed")
		}
		jobs := jobsOf(t, e, "s")
		if len(jobs) != 1 || jobs[0].NotBefore.IsZero() || jobs[0].NotBefore.After(reset.Add(time.Minute)) {
			t.Fatalf("wait must wake at the recorded reset, jobs=%+v", jobs)
		}
		// Past the reset and Claude's native grace (90s by default), the query is still throttled.
		clock.at = reset.Add(2 * time.Minute)
		refresh(t, e, q, throttled("a", clock.at, reset))
		scan(t, e)
		if b.count() != 1 || b.bindings[0].Executor != domain.ExecutorDesktop {
			t.Fatalf("reset fallback did not resume, calls=%d jobs=%+v", b.count(), jobsOf(t, e, "s"))
		}
		// The attempt failed again: that failure is newer than the reset.
		hostedEvent(t, e, "s", "StopFailure", clock.at.Add(time.Second))
		clock.at = clock.at.Add(time.Hour)
		refresh(t, e, q, throttled("a", clock.at, reset))
		scan(t, e)
		if b.count() != 1 {
			t.Fatalf("fallback repeated on a stale reset, calls=%d", b.count())
		}
	})
	t.Run("off waits for confirmed quota", func(t *testing.T) {
		e, q, b, clock, reset := setup(t)
		ctx := context.Background()
		snap, err := e.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.PatchPolicy(ctx, snap.PolicyDigest, map[string]json.RawMessage{"reset_fallback": json.RawMessage(`"off"`)}); err != nil {
			t.Fatal(err)
		}
		clock.at = reset.Add(time.Hour)
		refresh(t, e, q, throttled("a", clock.at, reset))
		scan(t, e)
		if b.count() != 0 {
			t.Fatal("reset_fallback=off must not send without confirmed quota")
		}
	})
	t.Run("rejected credential never falls back", func(t *testing.T) {
		e, q, b, clock, reset := setup(t)
		clock.at = reset.Add(time.Hour)
		a := throttled("a", clock.at, reset)
		a.QuotaError = "quota request returned HTTP 401"
		refresh(t, e, q, a)
		scan(t, e)
		if b.count() != 0 {
			t.Fatal("401 must not be treated as a throttled query")
		}
	})
}
