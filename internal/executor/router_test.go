package executor

import (
	"context"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/domain"
)

type stubBackend struct {
	name  string
	calls *[]string
}

func (s stubBackend) Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error) {
	*s.calls = append(*s.calls, s.name)
	return domain.BridgeResult{OK: true, Code: s.name}, nil
}
func (s stubBackend) Probe(context.Context) domain.BridgeResult {
	return domain.BridgeResult{OK: true, Code: s.name + "-probe"}
}

func TestRouterDispatchesByExecutorWithoutFallback(t *testing.T) {
	var calls []string
	r := NewRouter(stubBackend{"desktop", &calls}, stubBackend{"terminal", &calls})
	for _, executor := range []string{"", domain.ExecutorDesktop} {
		if result, _ := r.Resume(context.Background(), domain.Binding{Executor: executor}, "继续"); result.Code != "desktop" {
			t.Fatalf("%q routed to %s", executor, result.Code)
		}
	}
	if result, _ := r.Resume(context.Background(), domain.Binding{Executor: domain.ExecutorTerminal}, "继续"); result.Code != "terminal" {
		t.Fatalf("terminal binding routed to %s", result.Code)
	}
	for _, executor := range []string{"codex", "terminal", "Claude-Terminal"} {
		if result, _ := r.Resume(context.Background(), domain.Binding{Executor: executor}, "继续"); result.OK || result.Code != "executor_unsupported" {
			t.Fatalf("unsupported executor %q fell back to desktop", executor)
		}
		if result := r.Locate(context.Background(), domain.Binding{Executor: executor}); result.OK || result.Code != "executor_unsupported" {
			t.Fatalf("unsupported executor %q located on desktop", executor)
		}
	}
	if result := r.Preflight(context.Background(), domain.Binding{Executor: domain.ExecutorDesktop}); result.Code != "desktop-probe" {
		t.Fatal("preflight did not fall back to the same backend's probe")
	}
	if result := r.Locate(context.Background(), domain.Binding{}); result.Code != "locate_unavailable" {
		t.Fatal("locate invented for a backend without it")
	}
	none := NewRouter(nil, nil)
	for _, executor := range []string{"", domain.ExecutorTerminal} {
		if result, _ := none.Resume(context.Background(), domain.Binding{Executor: executor}, "继续"); result.OK || !strings.Contains(result.Code, "unconfigured") {
			t.Fatalf("missing %q executor accepted a send", executor)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("unexpected backend calls %v", calls)
	}
}

type stubDirect struct {
	code  string
	calls *[]string
}

func (s stubDirect) result() domain.BridgeResult {
	ok := s.code == "direct_ready" || s.code == "direct_delivered"
	return domain.BridgeResult{OK: ok, Submitted: s.code == "direct_delivered", Code: s.code, Channel: domain.ChannelDirect}
}
func (s stubDirect) Preflight(context.Context, domain.Binding) domain.BridgeResult { return s.result() }
func (s stubDirect) Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error) {
	*s.calls = append(*s.calls, "direct")
	return s.result(), nil
}

func TestRouterDirectChannel(t *testing.T) {
	var calls []string
	desktop := domain.Binding{Executor: domain.ExecutorDesktop, ExecutionDirect: true}
	r := NewRouter(stubBackend{"desktop", &calls}, stubBackend{"terminal", &calls}).WithDirect(stubDirect{"direct_delivered", &calls})
	if res, _ := r.Resume(context.Background(), desktop, "继续"); res.Code != "direct_delivered" {
		t.Fatalf("live session not sent directly: %+v", res)
	}
	// The policy switch keeps the UI channel.
	off := desktop
	off.ExecutionDirect = false
	if res, _ := r.Resume(context.Background(), off, "继续"); res.Code != "desktop" {
		t.Fatalf("direct used while disabled: %+v", res)
	}
	// Only "unavailable" hands over to the session's own UI/terminal channel.
	r.WithDirect(stubDirect{"direct_unavailable", &calls})
	if res, _ := r.Resume(context.Background(), desktop, "继续"); res.Code != "desktop" {
		t.Fatalf("unavailable did not fall back: %+v", res)
	}
	term := domain.Binding{Executor: domain.ExecutorTerminal, ExecutionDirect: true}
	if res := r.Preflight(context.Background(), term); res.Code != "terminal-probe" {
		t.Fatalf("terminal preflight fallback: %+v", res)
	}
	for _, code := range []string{"session_busy", "direct_target_ambiguous", "submitted_uncertain"} {
		r.WithDirect(stubDirect{code, &calls})
		if res, _ := r.Resume(context.Background(), desktop, "继续"); res.Code != code {
			t.Fatalf("%s fell back to the UI channel: %+v", code, res)
		}
		if res := r.Preflight(context.Background(), desktop); res.Code != code {
			t.Fatalf("%s preflight fell back: %+v", code, res)
		}
	}
	// An unsupported executor is refused before any channel is tried.
	if res, _ := r.Resume(context.Background(), domain.Binding{Executor: "codex", ExecutionDirect: true}, "继续"); res.Code != "executor_unsupported" {
		t.Fatalf("unsupported executor reached direct: %+v", res)
	}
	want := []string{"direct", "desktop", "direct", "desktop", "direct", "direct", "direct"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v", calls)
	}
}
