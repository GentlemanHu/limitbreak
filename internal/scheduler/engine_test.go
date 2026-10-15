package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

type fixtureQuota struct {
	mu       sync.Mutex
	accounts []domain.Account
	err      error
}

func (q *fixtureQuota) Refresh(context.Context) ([]domain.Account, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]domain.Account(nil), q.accounts...), q.err
}
func (q *fixtureQuota) Capabilities() domain.Capabilities {
	return domain.Capabilities{ProxyConnected: true}
}

type fixtureBridge struct {
	mu           sync.Mutex
	calls        int
	bindings     []domain.Binding
	messages     []string
	result       domain.BridgeResult
	err          error
	entered      chan struct{}
	release      chan struct{}
	probeGate    chan struct{}
	probeEntered chan struct{}
	probeResult  *domain.BridgeResult
	probes       int
	cancelled    chan struct{}
}

func (b *fixtureBridge) Probe(ctx context.Context) domain.BridgeResult {
	b.mu.Lock()
	b.probes++
	b.mu.Unlock()
	if b.probeEntered != nil {
		select {
		case b.probeEntered <- struct{}{}:
		default:
		}
	}
	if b.probeGate != nil {
		select {
		case <-b.probeGate:
		case <-ctx.Done():
			return domain.BridgeResult{Code: "cancelled"}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probeResult != nil {
		return *b.probeResult
	}
	return domain.BridgeResult{OK: true, AccessibilityGranted: true}
}
func (b *fixtureBridge) Resume(ctx context.Context, binding domain.Binding, message string) (domain.BridgeResult, error) {
	b.mu.Lock()
	b.calls++
	b.bindings = append(b.bindings, binding)
	b.messages = append(b.messages, message)
	b.mu.Unlock()
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			if b.cancelled != nil {
				select {
				case b.cancelled <- struct{}{}:
				default:
				}
			}
			return domain.BridgeResult{}, ctx.Err()
		}
	}
	return b.result, b.err
}
func (b *fixtureBridge) count() int { b.mu.Lock(); defer b.mu.Unlock(); return b.calls }
func policy() domain.Policy {
	p := config.Default(".").Policy
	p.Paused = false
	p.SafetySeconds = 0
	p.VerifySeconds = 5
	return p
}
func binding(id string) domain.Binding {
	return domain.Binding{SessionID: id, Kind: "code", Provider: "anthropic", Model: "opus", AccountIDs: []string{"a"}, TargetIdentifier: id, AutoResume: true, AcknowledgeSideEffects: true, ResumeOwner: "guard", NativeAutoResumeDisabledAcknowledged: true}
}
func allowed(id string) domain.Account {
	used := 10.0
	return domain.Account{ID: id, Provider: "anthropic", Models: []string{"opus"}, State: "allowed", CheckedAt: time.Now().UTC(), Windows: []domain.Window{{Name: "five_hour", UsedPercent: &used}}}
}
func fixture(t *testing.T) (*Engine, *fixtureQuota, *fixtureBridge) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	q := &fixtureQuota{}
	b := &fixtureBridge{result: domain.BridgeResult{OK: true, Submitted: true}}
	return New(s, q, b, policy()), q, b
}
func hook(t *testing.T, e *Engine, id, name string, at time.Time) domain.HookEvent {
	t.Helper()
	v := domain.HookEvent{EventID: newID(), SessionID: id, HookEventName: name, ObservedAt: at}
	if name == "StopFailure" {
		v.Error = "rate_limit_error"
	}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	return v
}
func queue(t *testing.T, e *Engine, id string) {
	t.Helper()
	if err := e.Bind(context.Background(), binding(id)); err != nil {
		t.Fatal(err)
	}
	hook(t, e, id, "StopFailure", time.Now().UTC())
}
func refresh(t *testing.T, e *Engine, q *fixtureQuota, accounts ...domain.Account) {
	t.Helper()
	q.mu.Lock()
	q.accounts = accounts
	q.mu.Unlock()
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func scan(t *testing.T, e *Engine) {
	t.Helper()
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
func state(t *testing.T, e *Engine, id string) domain.Session {
	t.Helper()
	s, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range s.Sessions {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("missing session %s", id)
	return domain.Session{}
}
func receive(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for bridge")
	}
}

func TestHooksDedupOrderingAndStateMachine(t *testing.T) {
	e, _, _ := fixture(t)
	now := time.Now().UTC()
	first := hook(t, e, "s", "UserPromptSubmit", now)
	gen := state(t, e, "s").Generation
	if err := e.HandleEvent(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if state(t, e, "s").Generation != gen {
		t.Fatal("duplicate prompt advanced generation")
	}
	hook(t, e, "s", "Stop", now.Add(-time.Second))
	if state(t, e, "s").State != domain.Running {
		t.Fatal("late Stop overwrote newer prompt")
	}
	hook(t, e, "s", "Stop", now.Add(time.Second))
	if state(t, e, "s").State != domain.Completed {
		t.Fatal("normal Stop did not complete")
	}
	hook(t, e, "s", "StopFailure", now.Add(2*time.Second))
	if state(t, e, "s").State != domain.Cooldown {
		t.Fatal("rate limit did not queue")
	}
	hook(t, e, "s", "SessionEnd", now.Add(3*time.Second))
	if state(t, e, "s").State != domain.Cancelled {
		t.Fatal("SessionEnd did not terminate")
	}
}
func TestStopFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		code, details string
		want          bool
	}{{"rate_limit_error", "", true}, {"429", "", true}, {"api_error", "Too many requests", true}, {"authentication_error", "rate limit", false}, {"overloaded_error", "529", false}, {"api_error", "internal server error", false}, {"", "quota exhausted", true}} {
		if got := IsRateLimit(domain.HookEvent{Error: tc.code, ErrorDetails: tc.details}); got != tc.want {
			t.Errorf("%s %s: %v", tc.code, tc.details, got)
		}
	}
	e, _, b := fixture(t)
	queue(t, e, "s")
	v := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "StopFailure", Error: "authentication_error", ObservedAt: time.Now().UTC()}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	scan(t, e)
	if state(t, e, "s").State != domain.Attention || b.count() != 0 {
		t.Fatal("non-quota failure treated as completion or retried")
	}
}
func TestFreshQuotaAndExplicitBindingRequired(t *testing.T) {
	for _, kind := range []string{"unknown", "stale", "missing-model", "wrong-provider", "disabled", "missing-window", "unknown-percent", "blocked-window", "pre-failure-evidence", "future-evidence", "alias-state", "unbound", "non-code", "auto-off", "unacknowledged", "paused"} {
		t.Run(kind, func(t *testing.T) {
			e, q, b := fixture(t)
			queue(t, e, "s")
			a := allowed("a")
			switch kind {
			case "unknown":
				a.State = "unknown"
			case "stale":
				a.CheckedAt = time.Now().Add(-time.Hour)
			case "missing-model":
				a.Models = nil
			case "wrong-provider":
				a.Provider = "other"
			case "disabled":
				a.Disabled = true
			case "missing-window":
				a.Windows = nil
			case "unknown-percent":
				a.Windows[0].UsedPercent = nil
			case "blocked-window":
				a.Windows[0].Blocked = true
			case "pre-failure-evidence":
				a.CheckedAt = time.Now().Add(-time.Second)
			case "future-evidence":
				a.CheckedAt = time.Now().Add(time.Hour)
			case "alias-state":
				a.State = "ready"
			case "paused":
				p := policy()
				p.Paused = true
				if err := e.SetPolicy(context.Background(), p); err != nil {
					t.Fatal(err)
				}
			default:
				if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
					s := tx.Sessions["s"]
					switch kind {
					case "unbound":
						s.Binding = nil
					case "non-code":
						s.Kind = "chat"
						s.Binding.Kind = "chat"
					case "auto-off":
						s.Binding.AutoResume = false
					case "unacknowledged":
						s.Binding.AcknowledgeSideEffects = false
					}
					tx.Sessions["s"] = s
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			refresh(t, e, q, a)
			scan(t, e)
			if b.count() != 0 {
				t.Fatalf("unsafe dispatch for %s", kind)
			}
		})
	}
}
func TestAXReturnNeedsHookConfirmation(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if b.count() != 1 || state(t, e, "s").State != domain.Verifying {
		t.Fatal("AX return falsely marked running")
	}
	hook(t, e, "s", "UserPromptSubmit", time.Now().UTC())
	if state(t, e, "s").State != domain.Running {
		t.Fatal("prompt hook did not confirm")
	}
	hook(t, e, "s", "Stop", time.Now().UTC())
	if state(t, e, "s").State != domain.Completed {
		t.Fatal("Stop did not complete")
	}
	scan(t, e)
	if b.count() != 1 {
		t.Fatal("confirmed job resent")
	}
}
func TestFreshStopAndStopFailureConfirmDispatch(t *testing.T) {
	for _, name := range []string{"Stop", "StopFailure", "SessionEnd"} {
		t.Run(name, func(t *testing.T) {
			e, q, b := fixture(t)
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			scan(t, e)
			hook(t, e, "s", name, time.Now().UTC())
			snap, err := e.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, j := range snap.Jobs {
				if j.Attempts == 1 && j.State == domain.Completed {
					found = true
				}
			}
			if !found || b.count() != 1 {
				t.Fatal("fresh hook did not confirm original dispatch")
			}
			if name == "StopFailure" && state(t, e, "s").State != domain.Cooldown {
				t.Fatal("StopFailure mistaken for Stop")
			}
		})
	}
}
func TestTimeoutAndUncertainSubmissionNeverResend(t *testing.T) {
	for _, kind := range []string{"timeout", "bridge-error", "not-submitted", "uncertain"} {
		t.Run(kind, func(t *testing.T) {
			e, q, b := fixture(t)
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			switch kind {
			case "bridge-error":
				b.err = errors.New("bridge crashed")
			case "not-submitted":
				b.result.Submitted = false
			case "uncertain":
				b.result.OK = false
				b.result.Submitted = true
			}
			scan(t, e)
			if kind == "timeout" {
				if err := e.store.Update(context.Background(), func(tx *store.Tx) error {
					for id, j := range tx.Jobs {
						expired := time.Now().Add(-time.Second)
						j.LeaseUntil = &expired
						tx.Jobs[id] = j
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			scan(t, e)
			scan(t, e)
			if b.count() != 1 || state(t, e, "s").State != domain.Attention {
				t.Fatal("uncertain dispatch retried or not marked attention")
			}
		})
	}
}
func TestReadOnlyRefreshPreservesWaitingGeneration(t *testing.T) {
	e, q, _ := fixture(t)
	queue(t, e, "s")
	g := state(t, e, "s").Generation
	a := allowed("a")
	a.State = "limited"
	reset := time.Now().Add(time.Hour)
	a.AvailableAt = &reset
	a.Windows[0].Blocked = true
	refresh(t, e, q, a)
	scan(t, e)
	before, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refresh(t, e, q, a)
	after, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state(t, e, "s").Generation != g || len(after.Jobs) != len(before.Jobs) || after.Jobs[0].ID != before.Jobs[0].ID || after.Jobs[0].State != domain.Cooldown {
		t.Fatal("refresh cancelled or recreated waiting job")
	}
}
func TestPoolEarliestResetUsesProxyMaxAndNeverAssumesRecovery(t *testing.T) {
	e, q, b := fixture(t)
	bind := binding("s")
	bind.AccountIDs = []string{"a", "b"}
	if err := e.Bind(context.Background(), bind); err != nil {
		t.Fatal(err)
	}
	hook(t, e, "s", "StopFailure", time.Now().UTC())
	now := time.Now().UTC()
	short := now.Add(time.Hour)
	long := now.Add(5 * time.Hour)
	other := now.Add(2 * time.Hour)
	a := allowed("a")
	a.State = "limited"
	a.AvailableAt = &long
	a.Windows = []domain.Window{{Blocked: true, ResetAt: &short}, {Blocked: true, ResetAt: &long}}
	bb := allowed("b")
	bb.State = "limited"
	bb.AvailableAt = &other
	bb.Windows[0].Blocked = true
	refresh(t, e, q, a, bb)
	scan(t, e)
	snap, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Jobs[0].NotBefore.Equal(other) || b.count() != 0 {
		t.Fatal("pool earliest reset or account max reset violated")
	}
	past := now.Add(-time.Minute)
	a.AvailableAt = &past
	bb.AvailableAt = &past
	refresh(t, e, q, a, bb)
	scan(t, e)
	if b.count() != 0 {
		t.Fatal("expired reset guessed available")
	}
	bb = allowed("b")
	refresh(t, e, q, a, bb)
	scan(t, e)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls != 1 || len(b.bindings[0].AccountIDs) != 1 || b.bindings[0].AccountIDs[0] != "b" {
		t.Fatal("did not dispatch explicitly eligible account b")
	}
}
func TestCancelAndNewPromptCancelBridgeContext(t *testing.T) {
	for _, action := range []string{"cancel", "prompt"} {
		t.Run(action, func(t *testing.T) {
			e, q, b := fixture(t)
			b.entered = make(chan struct{}, 1)
			b.release = make(chan struct{})
			b.cancelled = make(chan struct{}, 1)
			queue(t, e, "s")
			refresh(t, e, q, allowed("a"))
			var wg sync.WaitGroup
			if err := e.scan(context.Background(), &wg); err != nil {
				t.Fatal(err)
			}
			receive(t, b.entered)
			if action == "cancel" {
				if err := e.Cancel(context.Background(), "s"); err != nil {
					t.Fatal(err)
				}
			} else {
				hook(t, e, "s", "UserPromptSubmit", time.Now().UTC())
			}
			receive(t, b.cancelled)
			wg.Wait()
			scan(t, e)
			want := domain.Cancelled
			if action == "prompt" {
				want = domain.Running
			}
			if b.count() != 1 || state(t, e, "s").State != want {
				t.Fatal("old generation completion overwrote cancellation/new prompt")
			}
		})
	}
}
func TestCancelBeforeResumePreflightGenerationCheck(t *testing.T) {
	e, q, b := fixture(t)
	b.probeGate = make(chan struct{})
	b.probeEntered = make(chan struct{}, 1)
	queue(t, e, "s")
	refresh(t, e, q, allowed("a"))
	var wg sync.WaitGroup
	if err := e.scan(context.Background(), &wg); err != nil {
		t.Fatal(err)
	}
	receive(t, b.probeEntered)
	if err := e.Cancel(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	close(b.probeGate)
	wg.Wait()
	if b.count() != 0 {
		t.Fatal("cancelled generation was sent after probe")
	}
}
func TestOldHookCannotConfirmNewDispatch(t *testing.T) {
	e, q, _ := fixture(t)
	queue(t, e, "s")
	old := time.Now().UTC()
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	hook(t, e, "s", "Stop", old)
	if state(t, e, "s").State != domain.Verifying {
		t.Fatal("pre-dispatch hook confirmed new dispatch")
	}
}
func TestSessionTitleAndModelMismatch(t *testing.T) {
	e, q, b := fixture(t)
	queue(t, e, "s")
	v := domain.HookEvent{EventID: newID(), SessionID: "s", HookEventName: "ModelChange", NewModel: "sonnet", SessionTitle: "Real title", ObservedAt: time.Now().UTC()}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	s := state(t, e, "s")
	refresh(t, e, q, allowed("a"))
	scan(t, e)
	if s.Title != "Real title" || s.Model != "sonnet" || s.State != domain.Attention || b.count() != 0 {
		t.Fatal("model mismatch or title handling incorrect")
	}
}
