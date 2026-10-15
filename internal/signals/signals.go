// Package signals polls external reset feeds and turns announced quota resets
// into reset signals. A signal only makes LimitBreak query the matching
// provider's accounts sooner; it never authorizes a send by itself.
//
// Adding a feed: implement Source, register its factory in Factories and add
// its type to config.SignalSourceTypes. Feeds that can push instead of being
// polled should call the authenticated webhook (POST /api/signals) instead.
package signals

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

// Source polls one reset feed. Poll receives the previous persisted state and
// returns the updated state plus the signals that are new since then. It must
// deduplicate with prev.LastID and must not emit anything on the very first
// poll (prev.LastID == "") unless the reset is recent.
type Source interface {
	Descriptor() domain.SignalSourceState
	Poll(ctx context.Context, prev domain.SignalSourceState, now time.Time) (Result, error)
}

type Result struct {
	State   domain.SignalSourceState
	Signals []domain.ResetSignal
}

// RetryError asks the runner not to poll again before At (HTTP 429/503).
type RetryError struct {
	At  time.Time
	Err error
}

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

type Factory func(config.SignalSource) (Source, error)

// Factories is the closed registry of signal source types.
var Factories = map[string]Factory{"codex-resets": NewCodexResets}

// Sink is implemented by the scheduler engine.
type Sink interface {
	SignalStates(context.Context) (map[string]domain.SignalSourceState, error)
	SaveSignalState(context.Context, domain.SignalSourceState) error
	// ApplyResetSignal makes the provider's accounts due for a quota query.
	// followUp marks a quiet re-check of an already recorded signal.
	ApplyResetSignal(ctx context.Context, s domain.ResetSignal, followUp bool) (int, error)
	HasProvider(context.Context, string) (bool, error)
}

// FollowUps are the quiet re-checks after a reset signal. Announcements can
// precede the reset reaching every account, and a proxy may keep its own
// cooldown for a while, so one query right after the signal is not enough.
var FollowUps = []time.Duration{3 * time.Minute, 10 * time.Minute}

const idleRecheck = 5 * time.Minute

type configured struct {
	source   Source
	mode     string
	interval time.Duration
	failures int
	next     time.Time
}

type pending struct {
	at       time.Time
	signal   domain.ResetSignal
	followUp bool
}

type Runner struct {
	sink    Sink
	sources []*configured
	now     func() time.Time
	mu      sync.Mutex
	queue   []pending
	fired   map[string]bool
	wake    chan struct{}
}

// New builds a runner for every effective signal source in c.
func New(c config.Config, sink Sink) (*Runner, error) {
	r := &Runner{sink: sink, now: func() time.Time { return time.Now().UTC() }, fired: map[string]bool{}, wake: make(chan struct{}, 1)}
	for _, s := range c.EffectiveSignalSources() {
		factory, ok := Factories[s.Type]
		if !ok {
			return nil, fmt.Errorf("signal source type %q is not implemented", s.Type)
		}
		source, err := factory(s)
		if err != nil {
			return nil, err
		}
		r.sources = append(r.sources, &configured{source: source, mode: s.Mode, interval: time.Duration(s.PollSeconds) * time.Second})
	}
	return r, nil
}

// Run polls until ctx ends. It returns immediately when nothing is configured.
func (r *Runner) Run(ctx context.Context) {
	if len(r.sources) == 0 {
		return
	}
	for ctx.Err() == nil {
		r.tick(ctx)
		delay := time.Until(r.nextDue())
		if delay < time.Second {
			delay = time.Second
		}
		// A bounded watchdog re-evaluates wall time after sleep.
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-r.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (r *Runner) nextDue() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var due time.Time
	for _, s := range r.sources {
		if due.IsZero() || s.next.Before(due) {
			due = s.next
		}
	}
	for _, p := range r.queue {
		if p.at.Before(due) {
			due = p.at
		}
	}
	return due
}

func (r *Runner) tick(ctx context.Context) {
	now := r.now()
	states, err := r.sink.SignalStates(ctx)
	if err != nil {
		return
	}
	for _, s := range r.sources {
		if now.Before(s.next) {
			continue
		}
		r.poll(ctx, s, states[s.source.Descriptor().ID], now)
	}
	r.mu.Lock()
	var due, later []pending
	for _, p := range r.queue {
		if now.Before(p.at) {
			later = append(later, p)
		} else {
			due = append(due, p)
		}
	}
	r.queue = later
	r.mu.Unlock()
	for _, p := range due {
		_, _ = r.sink.ApplyResetSignal(ctx, p.signal, p.followUp)
	}
}

func (r *Runner) poll(ctx context.Context, s *configured, prev domain.SignalSourceState, now time.Time) {
	desc := s.source.Descriptor()
	base := merge(desc, prev)
	base.Enabled = s.mode != config.SignalModeOff
	if s.mode == config.SignalModeOff {
		base.State, base.Error, base.NextPollAt = domain.SignalStateIdle, "已关闭", nil
		s.next = now.Add(24 * time.Hour)
		_ = r.sink.SaveSignalState(ctx, base)
		return
	}
	if s.mode == config.SignalModeAuto {
		if wanted, err := r.sink.HasProvider(ctx, desc.Provider); err == nil && !wanted {
			s.next = now.Add(idleRecheck)
			next := s.next
			base.State, base.Error, base.NextPollAt = domain.SignalStateIdle, "没有 "+desc.Provider+" 账号，暂停轮询", &next
			_ = r.sink.SaveSignalState(ctx, base)
			return
		}
	}
	res, err := s.source.Poll(ctx, base, now)
	polled := now
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.failures++
		backoff := s.interval
		for i := 1; i < min(s.failures, 6); i++ {
			backoff *= 2
		}
		backoff = min(backoff, 30*time.Minute)
		s.next = now.Add(backoff)
		var retry *RetryError
		if errors.As(err, &retry) && retry.At.After(s.next) {
			s.next = retry.At
		}
		next := s.next
		base.State, base.Error, base.LastPollAt, base.NextPollAt = domain.SignalStateError, err.Error(), &polled, &next
		_ = r.sink.SaveSignalState(ctx, base)
		return
	}
	s.failures = 0
	s.next = now.Add(s.interval)
	state := merge(desc, res.State)
	state.Enabled = true
	next := s.next
	state.State, state.Error, state.LastPollAt, state.NextPollAt = domain.SignalStateOK, "", &polled, &next
	for _, signal := range res.Signals {
		signal.Source, signal.Provider = desc.ID, desc.Provider
		if signal.ReceivedAt.IsZero() {
			signal.ReceivedAt = now
		}
		if _, err := r.sink.ApplyResetSignal(ctx, signal, false); err == nil {
			t := now
			state.LastTriggeredAt = &t
			r.followUps(signal, now)
		}
	}
	if sched := state.Scheduled; sched != nil && sched.At != nil && sched.At.After(now) && !r.seen("scheduled:"+desc.ID+":"+sched.ID) {
		// A scheduled reset is checked once shortly after its announced time.
		r.enqueue(pending{at: sched.At.Add(time.Minute), signal: domain.ResetSignal{ID: sched.ID, Source: desc.ID, Provider: desc.Provider, Kind: domain.SignalResetScheduled, At: *sched.At, ReceivedAt: now, Text: sched.Text, URL: sched.URL}})
	}
	_ = r.sink.SaveSignalState(ctx, state)
}

func (r *Runner) followUps(s domain.ResetSignal, now time.Time) {
	for _, d := range FollowUps {
		r.enqueue(pending{at: now.Add(d), signal: s, followUp: true})
	}
}

func (r *Runner) seen(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fired[key] {
		return true
	}
	r.fired[key] = true
	return false
}

func (r *Runner) enqueue(p pending) {
	r.mu.Lock()
	r.queue = append(r.queue, p)
	sort.SliceStable(r.queue, func(i, j int) bool { return r.queue[i].at.Before(r.queue[j].at) })
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// merge keeps the descriptor's identity and attribution over persisted values.
func merge(desc, state domain.SignalSourceState) domain.SignalSourceState {
	state.ID, state.Type, state.Name, state.Provider = desc.ID, desc.Type, desc.Name, desc.Provider
	state.Attribution, state.AttributionURL = desc.Attribution, desc.AttributionURL
	return state
}
