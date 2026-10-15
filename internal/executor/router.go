// Package executor delivers continue messages to the app hosting a session.
package executor

import (
	"context"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// Backend is one delivery channel. Preflight, Locate and ValidateTarget are
// optional; a backend without them reports the capability as unavailable.
type Backend interface {
	Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error)
	Probe(context.Context) domain.BridgeResult
}

// Direct is the optional silent channel: the session's own messaging socket.
// It reports direct.CodeUnavailable when the session has no live process.
type Direct interface {
	Preflight(context.Context, domain.Binding) domain.BridgeResult
	Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error)
}

// Waker is an optional backend capability: open the session in the background
// so its host starts the session's process, without typing anything.
type Waker interface {
	Wake(context.Context, domain.Binding, string) domain.BridgeResult
}

// Router dispatches every operation by binding.Executor and never delivers to
// another application: a binding for an unsupported host is refused. When the
// binding allows it and the very same session process is live, Preflight and
// Resume go through the direct channel. When the process is not running and
// the backend can wake the session, Resume wakes it, waits for its socket and
// still delivers silently. Only when that fails, before anything is written,
// does the operation go to the session's UI or terminal channel.
type Router struct {
	desktop  Backend
	terminal Backend
	direct   Direct
	// clients are the configured generic client commands by client ID.
	clients map[string]Backend
	// How long and how often to look for the woken session's socket.
	wakeWait time.Duration
	wakePoll time.Duration
}

func NewRouter(desktop, terminal Backend) *Router {
	return &Router{desktop: desktop, terminal: terminal, wakeWait: 20 * time.Second, wakePoll: 500 * time.Millisecond}
}

// WithDirect enables the direct channel for bindings with ExecutionDirect.
func (r *Router) WithDirect(d Direct) *Router {
	r.direct = d
	return r
}

// WithClients registers generic client backends, keyed by client ID.
func (r *Router) WithClients(clients map[string]Backend) *Router {
	r.clients = clients
	return r
}

// useDirect never applies to generic clients: their sessions have no Claude
// messaging socket, and a colliding ID must not reach a Claude session.
func (r *Router) useDirect(b domain.Binding) bool {
	return r.direct != nil && b.ExecutionDirect && !strings.HasPrefix(b.Executor, domain.ClientExecutorPrefix)
}

func directUnavailable(res domain.BridgeResult) bool {
	return res.Channel == domain.ChannelDirect && res.Code == "direct_unavailable" && !res.Submitted
}

// The wake result codes that mean "not now": the same refusal would stop the
// UI channel, so the job is retried later instead of falling back.
var wakeDeferred = map[string]bool{
	"user_active": true, "claude_in_use": true, "screen_locked": true, "screen_inactive": true, "console_inactive": true,
	"lock_state_unverified": true, "idle_state_unverified": true, "cancelled": true,
}

// waker is the backend's wake capability for a hosted desktop deep-link session.
func (r *Router) waker(b domain.Binding, backend Backend) Waker {
	w, ok := backend.(Waker)
	if !ok || !r.useDirect(b) || b.Kind != "code" || (b.Executor != "" && b.Executor != domain.ExecutorDesktop) || !domain.ValidDesktopDeepLink(b.DeepLink) {
		return nil
	}
	return w
}

// wakeAndDeliver opens the session in the background, waits for its process to
// appear, and delivers through the direct channel. ok is false when the
// session never became reachable and nothing was written.
func (r *Router) wakeAndDeliver(ctx context.Context, w Waker, b domain.Binding, message string) (domain.BridgeResult, bool, error) {
	woke := w.Wake(ctx, b, message)
	if !woke.OK {
		if wakeDeferred[woke.Code] || ctx.Err() != nil {
			return woke, true, nil
		}
		return woke, false, nil
	}
	end := time.Now().Add(r.wakeWait)
	tick := time.NewTicker(r.wakePoll)
	defer tick.Stop()
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return domain.BridgeResult{Code: "cancelled", Message: "唤醒会话后等待期间已取消；未发送。", Woken: true}, true, ctx.Err()
		case <-tick.C:
		}
		probe := r.direct.Preflight(ctx, b)
		if directUnavailable(probe) || probe.Code == "session_busy" {
			continue
		}
		if !probe.OK {
			return probe, true, nil
		}
		res, err := r.direct.Resume(ctx, b, message)
		if err == nil && directUnavailable(res) && ctx.Err() == nil {
			continue
		}
		res.Woken = true
		return res, true, err
	}
	return woke, false, nil
}

func unsupported(code, message string) domain.BridgeResult {
	return domain.BridgeResult{Code: code, Message: message, ScreenLocked: true}
}

func (r *Router) backend(b domain.Binding) (Backend, *domain.BridgeResult) {
	var chosen Backend
	switch b.Executor {
	case "", domain.ExecutorDesktop:
		chosen = r.desktop
	case domain.ExecutorTerminal:
		chosen = r.terminal
	default:
		if id, ok := strings.CutPrefix(b.Executor, domain.ClientExecutorPrefix); ok && domain.ValidClientID(id) {
			if client := r.clients[id]; client != nil {
				return client, nil
			}
			res := unsupported("executor_unconfigured", "客户端 "+id+" 未在配置的 clients 中，无法自动继续。")
			return nil, &res
		}
		res := unsupported("executor_unsupported", "该会话所在的应用暂不支持自动继续。")
		return nil, &res
	}
	if chosen == nil {
		res := unsupported("executor_unconfigured", "该会话的执行器未配置。")
		return nil, &res
	}
	return chosen, nil
}

func (r *Router) Resume(ctx context.Context, b domain.Binding, message string) (domain.BridgeResult, error) {
	backend, problem := r.backend(b)
	if problem != nil {
		return *problem, nil
	}
	woken := false
	if r.useDirect(b) {
		res, err := r.direct.Resume(ctx, b, message)
		if err != nil || !directUnavailable(res) || ctx.Err() != nil {
			return res, err
		}
		if w := r.waker(b, backend); w != nil {
			res, done, err := r.wakeAndDeliver(ctx, w, b, message)
			if done || ctx.Err() != nil {
				return res, err
			}
			woken = res.OK
		}
	}
	res, err := backend.Resume(ctx, b, message)
	res.Woken = res.Woken || woken
	return res, err
}

// Probe reports the desktop App, which owns the permission and screen state.
func (r *Router) Probe(ctx context.Context) domain.BridgeResult {
	if r.desktop == nil {
		return unsupported("bridge_unavailable", "桌面执行器未配置。")
	}
	return r.desktop.Probe(ctx)
}

func (r *Router) Preflight(ctx context.Context, b domain.Binding) domain.BridgeResult {
	backend, problem := r.backend(b)
	if problem != nil {
		return *problem
	}
	if r.useDirect(b) {
		res := r.direct.Preflight(ctx, b)
		if !directUnavailable(res) {
			return res
		}
		// No live process yet: Resume will wake the session first, which needs
		// neither Accessibility nor the UI channel.
		if r.waker(b, backend) != nil {
			return domain.BridgeResult{OK: true, Code: "direct_wake_ready", Message: "会话进程未运行；将在后台打开会话以启动进程，再静默直发。",
				AccessibilityGranted: true, Channel: domain.ChannelDirect}
		}
	}
	if p, ok := backend.(interface {
		Preflight(context.Context, domain.Binding) domain.BridgeResult
	}); ok {
		return p.Preflight(ctx, b)
	}
	return backend.Probe(ctx)
}

func (r *Router) Locate(ctx context.Context, b domain.Binding) domain.BridgeResult {
	backend, problem := r.backend(b)
	if problem != nil {
		return *problem
	}
	if l, ok := backend.(interface {
		Locate(context.Context, domain.Binding) domain.BridgeResult
	}); ok {
		return l.Locate(ctx, b)
	}
	return unsupported("locate_unavailable", "该执行器不支持定位。")
}

func (r *Router) ValidateTarget(ctx context.Context, b domain.Binding) domain.BridgeResult {
	backend, problem := r.backend(b)
	if problem != nil {
		return *problem
	}
	if v, ok := backend.(interface {
		ValidateTarget(context.Context, domain.Binding) domain.BridgeResult
	}); ok {
		return v.ValidateTarget(ctx, b)
	}
	return unsupported("target_preflight_unavailable", "该执行器不支持目标核验。")
}
