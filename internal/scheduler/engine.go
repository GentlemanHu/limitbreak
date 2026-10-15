package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"quota-session-orchestrator/internal/adapters"
	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

type QuotaSource = adapters.QuotaSource
type Bridge = adapters.ResumeExecutor

// Clock injects observation/lease time into the real production state machine.
// TimerClock optionally makes Run deterministic; Now-only clocks remain compatible.
type Clock interface{ Now() time.Time }
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type activeCall struct {
	generation int64
	cancel     context.CancelFunc
}
type Engine struct {
	store          *store.Store
	clock          Clock
	quota          QuotaSource
	bridge         Bridge
	initial        domain.Policy
	initialSources []domain.SourceHealth
	owner          string
	mu             sync.Mutex
	active         map[string]activeCall
	refreshMu      sync.Mutex
	policyMu       sync.Mutex
	runMu          sync.Mutex
	running        bool
	wake           chan struct{}
	scheduleWake   chan struct{}
	lastRefresh    time.Time
	pollInterval   time.Duration
	lookup         ProcessLookup
}

// SetProcessLookup enables releasing the running budget of sessions whose
// Claude process has provably ended. Call before Run.
func (e *Engine) SetProcessLookup(lookup ProcessLookup) {
	e.mu.Lock()
	e.lookup = lookup
	e.mu.Unlock()
}

func New(s *store.Store, q QuotaSource, b Bridge, p domain.Policy) *Engine {
	return NewWithClock(s, q, b, p, realClock{})
}

func NewWithClock(s *store.Store, q QuotaSource, b Bridge, p domain.Policy, clock Clock) *Engine {
	if clock == nil {
		clock = realClock{}
	}
	sources := []domain.SourceHealth{}
	if q != nil {
		sources = append(sources, q.Capabilities().Sources...)
		if len(sources) == 0 {
			sources = []domain.SourceHealth{{ID: domain.DefaultSourceID, Type: "cliproxyapi"}}
		}
		for i := range sources {
			sources[i].State = "unknown"
			sources[i].Connected = false
			sources[i].Error = ""
		}
	}
	return &Engine{store: s, clock: clock, quota: q, bridge: b, initial: p, initialSources: sources, owner: newID(), active: map[string]activeCall{}, wake: make(chan struct{}, 1), scheduleWake: make(chan struct{}, 1), pollInterval: 180 * time.Second}
}

func (e *Engine) now() time.Time { return e.clock.Now().UTC() }

// Step performs one production scan and waits for that scan's dispatch to settle.
// It starts no polling loop and does not replace any safety checks.
func (e *Engine) Step(ctx context.Context) error {
	if err := e.init(ctx); err != nil {
		return err
	}
	var workers sync.WaitGroup
	err := e.scan(ctx, &workers)
	workers.Wait()
	return err
}

// SetPollInterval applies to the next polling cycle. Dispatch scanning is independent.
func (e *Engine) SetPollInterval(interval time.Duration) error {
	if interval < 30*time.Second {
		return errors.New("quota polling interval must be at least 30 seconds")
	}
	e.mu.Lock()
	e.pollInterval = interval
	e.mu.Unlock()
	e.reschedule()
	return nil
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("secure ID generation: %v", err))
	}
	return hex.EncodeToString(b[:])
}
func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) init(ctx context.Context) error {
	if e.store == nil {
		return errors.New("store is required")
	}
	if err := config.ValidatePolicy(e.initial); err != nil {
		return err
	}
	return e.store.Update(ctx, func(t *store.Tx) error {
		if t.Policy == nil {
			p := e.initial
			t.Policy = &p
		}
		if t.Capabilities.Sources == nil {
			t.Capabilities.Sources = append([]domain.SourceHealth{}, e.initialSources...)
		}
		e.migrateSources(t)
		ensureLifecycle(t)
		return nil
	})
}

// Run owns only timer loops; it never relies on a browser tab remaining open.
func (e *Engine) Run(ctx context.Context) error {
	e.runMu.Lock()
	if e.running {
		e.runMu.Unlock()
		return errors.New("engine is already running")
	}
	e.running = true
	e.runMu.Unlock()
	defer func() { e.runMu.Lock(); e.running = false; e.runMu.Unlock() }()
	if err := e.init(ctx); err != nil {
		return err
	}
	if aware, ok := e.quota.(interface {
		SetPolicy(context.Context, domain.Policy) error
	}); ok {
		state, err := e.store.Read(ctx)
		if err != nil {
			return err
		}
		if err := aware.SetPolicy(ctx, *state.Policy); err != nil {
			return err
		}
	}
	// A new process must obtain fresh proxy evidence before dispatching persisted jobs.
	if err := e.store.Update(ctx, func(t *store.Tx) error {
		t.Capabilities.ProxyConnected = false
		for i := range t.Capabilities.Sources {
			t.Capabilities.Sources[i].Connected = false
			t.Capabilities.Sources[i].State = "unknown"
		}
		return nil
	}); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	// The refresh timer has its own wake channel; scan wakes never poll the network.
	workers.Add(1)
	go func() { defer workers.Done(); e.runRefresh(runCtx) }()
	defer func() {
		cancel()
		e.mu.Lock()
		for _, a := range e.active {
			a.cancel()
		}
		e.mu.Unlock()
		workers.Wait()
	}()
	for {
		if err := e.scan(runCtx, &workers); err != nil && runCtx.Err() == nil {
			// Transient storage failure must not end lifecycle observation or lose
			// durable queued events; retry without dispatching an uncommitted claim.
			_ = e.recordError(runCtx, "scheduler_scan", err)
		}
		timer := e.newTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C():
		case <-e.wake:
			timer.Stop()
		}
	}
}
func (e *Engine) recordError(ctx context.Context, kind string, err error) error {
	return e.store.Update(ctx, func(t *store.Tx) error { t.Log(e.now(), kind, "", err.Error()); return nil })
}

// Refresh replaces quota evidence, but never cancels or recreates a waiting job.
func (e *Engine) Refresh(ctx context.Context) error {
	if err := e.init(ctx); err != nil {
		return err
	}
	if e.quota == nil {
		return errors.New("quota source is required")
	}
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()
	defer func() {
		e.mu.Lock()
		e.lastRefresh = e.now()
		e.mu.Unlock()
		e.reschedule()
	}()
	accounts, err := e.quota.Refresh(ctx)
	caps := e.quota.Capabilities()
	if err != nil {
		if saveErr := e.rejectQuota(ctx, caps); saveErr != nil {
			return saveErr
		}
		return err
	}
	if len(caps.Sources) == 0 {
		health := domain.SourceHealth{ID: domain.DefaultSourceID, Type: "cliproxyapi", Connected: caps.ProxyConnected, State: "unknown", CheckedAt: e.now(), AccountCount: len(accounts)}
		if caps.ProxyConnected {
			health.State = "healthy"
			success := e.now()
			health.LastSuccessAt = &success
		}
		caps.Sources = []domain.SourceHealth{health}
	}
	seen := map[string]bool{}
	for i := range accounts {
		a := &accounts[i]
		if a.ID == "" || seen[a.ID] {
			if saveErr := e.rejectQuota(ctx, caps); saveErr != nil {
				return saveErr
			}
			return errors.New("quota source returned missing or duplicate account ID")
		}
		if a.SourceID == "" {
			a.SourceID = domain.DefaultSourceID
		}
		seen[a.ID] = true
	}
	if err = e.store.Update(ctx, func(t *store.Tx) error {
		keepLastKnown(accounts, t.Accounts)
		t.Accounts = accounts
		applyCapabilities(t, caps)
		return nil
	}); err != nil {
		_ = e.store.Update(ctx, func(t *store.Tx) error { t.Capabilities.ProxyConnected = false; return nil })
		return err
	}
	e.signal()
	return nil
}
func (e *Engine) Bind(ctx context.Context, b domain.Binding) error {
	if err := e.init(ctx); err != nil {
		return err
	}
	if err := validateBinding(b); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.store.Update(ctx, func(t *store.Tx) error {
		return e.bindInTx(t, b, e.now())
	})
	if err == nil {
		e.cancelActive(b.SessionID)
		e.signal()
	}
	return err
}
func (e *Engine) Cancel(ctx context.Context, sessionID string) error {
	if err := e.init(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.store.Update(ctx, func(t *store.Tx) error {
		return cancelInTx(t, sessionID, e.now())
	})
	if err == nil {
		e.cancelActive(sessionID)
		e.signal()
	}
	return err
}

// cancelActive requires e.mu. Bridge calls must honor context cancellation.
func (e *Engine) cancelActive(id string) {
	if a, ok := e.active[id]; ok {
		a.cancel()
	}
}
func (e *Engine) SetPolicy(ctx context.Context, p domain.Policy) error {
	defer e.reschedule()
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	if err := config.ValidatePolicy(p); err != nil {
		return err
	}
	if err := e.init(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	err := e.store.Update(ctx, func(t *store.Tx) error {
		return setPolicyInTx(t, p, e.now())
	})
	if err == nil {
		for _, a := range e.active {
			a.cancel()
		}
		e.signal()
	}
	e.mu.Unlock()
	if err == nil {
		if aware, ok := e.quota.(interface {
			SetPolicy(context.Context, domain.Policy) error
		}); ok {
			return aware.SetPolicy(ctx, p)
		}
	}
	return err
}

const recoveredHint = "额度已确认恢复；上次继续未确认送达，不会自动重发，可在会话设置里手动发送一次"

// unconfirmedSend reports an attention reason left by a send whose delivery
// could not be confirmed.
func unconfirmedSend(reason string) bool {
	return strings.HasPrefix(reason, "dispatch confirmation timed out") || strings.HasPrefix(reason, "submission failed or uncertain")
}

func (e *Engine) Snapshot(ctx context.Context) (domain.Snapshot, error) {
	if err := e.init(ctx); err != nil {
		return domain.Snapshot{}, err
	}
	t, err := e.store.Read(ctx)
	if err != nil {
		return domain.Snapshot{}, err
	}
	for i := range t.Accounts {
		if t.Accounts[i].SourceID == "" {
			t.Accounts[i].SourceID = domain.DefaultSourceID
		}
	}
	sources := append([]domain.SourceHealth{}, t.Capabilities.Sources...)
	s := domain.Snapshot{Adapters: adapters.Descriptors(), Sources: sources, Time: e.now(), Accounts: t.Accounts, Sessions: []domain.Session{}, Jobs: []domain.Job{}, Audit: t.Audit, Capabilities: t.Capabilities, Policy: *t.Policy, PolicyDigest: store.PolicyDigest(*t.Policy), Signals: sortedSignals(t)}
	policy := config.EffectivePolicy(*t.Policy)
	freshness := time.Duration(t.Policy.FreshnessSeconds) * time.Second
	for _, v := range t.Sessions {
		var refs []domain.ResourceRef
		mode := "any"
		if v.Binding != nil {
			refs, mode = v.Binding.MonitorResources, v.Binding.MonitorMode
			if len(refs) == 0 {
				for _, id := range v.Binding.AccountIDs {
					refs = append(refs, domain.ResourceRef{AccountID: id})
				}
			}
		}
		if legacyGuard(v) {
			v.Pool = &domain.PoolView{Model: v.Binding.Model, Provider: v.Binding.Provider, AccountIDs: v.Binding.AccountIDs, Basis: "legacy"}
		} else {
			pool := resolvePool(t.Accounts, v, policy)
			v.Pool = &pool
			if len(refs) == 0 {
				for _, id := range pool.AccountIDs {
					refs = append(refs, domain.ResourceRef{AccountID: id})
				}
			}
		}
		if len(refs) > 0 {
			v.Monitor = domain.ObserveResources(refs, mode, t.Accounts, s.Time, freshness)
		}
		v.Host = hostView(t.Accounts, v, policy)
		v.ReasonText = domain.Explain(v.Reason)
		if v.State == domain.Attention && unconfirmedSend(v.Reason) && v.Monitor != nil && v.Monitor.State == "allowed" {
			// View only: the job is never resent automatically.
			v.ReasonText += "。" + recoveredHint
		}
		s.Sessions = append(s.Sessions, v)
	}
	for _, v := range t.Jobs {
		v.ReasonText = domain.Explain(v.Reason)
		s.Jobs = append(s.Jobs, v)
	}
	s.Audit = make([]domain.Audit, len(t.Audit))
	for i, a := range t.Audit {
		if a.Kind == "bridge_result" {
			a.Text = domain.Explain(a.Message)
		}
		s.Audit[i] = a
	}
	sort.Slice(s.Sessions, func(i, j int) bool { return s.Sessions[i].ID < s.Sessions[j].ID })
	sort.Slice(s.Jobs, func(i, j int) bool {
		if s.Jobs[i].CreatedAt.Equal(s.Jobs[j].CreatedAt) {
			return s.Jobs[i].ID < s.Jobs[j].ID
		}
		return s.Jobs[i].CreatedAt.Before(s.Jobs[j].CreatedAt)
	})
	return s, nil
}
func invalidate(t *store.Tx, id, reason string, now time.Time) {
	for key, j := range t.Jobs {
		if j.SessionID == id && (j.State == domain.Cooldown || j.State == domain.Ready || j.State == domain.Dispatching || j.State == domain.Verifying) {
			j.State = domain.Cancelled
			j.Reason = reason
			j.UpdatedAt = now
			j.LeaseUntil = nil
			t.Jobs[key] = j
		}
	}
}
func enqueue(t *store.Tx, s domain.Session, now time.Time) {
	id := fmt.Sprintf("%s/%d", s.ID, s.Generation)
	t.Jobs[id] = domain.Job{ID: id, SessionID: s.ID, Generation: s.Generation, State: domain.Cooldown, NotBefore: now, Reason: s.Reason, CreatedAt: now, UpdatedAt: now}
}
