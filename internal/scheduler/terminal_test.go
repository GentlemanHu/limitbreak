package scheduler

import (
	"context"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

const (
	cliA     = "11111111-2222-4333-8444-555555555555"
	cliB     = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	cliCwd   = "/Users/u/project"
	cliExec  = "/Users/u/.local/bin/claude"
	cliSock  = "/private/tmp/tmux-501/default"
	cliModel = "opus[1m]"
)

func terminalOrigin(pid int, tmux bool, at time.Time) *domain.SessionOrigin {
	o := &domain.SessionOrigin{App: domain.AppTerminal, Entrypoint: "cli", TermProgram: "iTerm.app", ClaudePID: pid, Ancestors: []int{pid, pid - 1}, ExecPath: cliExec, ObservedAt: at}
	if tmux {
		o.TmuxSocket, o.TmuxServerPID, o.TmuxPane = cliSock, pid-2, "%1"
	}
	return o
}

func terminalHook(t *testing.T, e *Engine, id, name string, origin *domain.SessionOrigin, at time.Time, edit func(*domain.HookEvent)) {
	t.Helper()
	v := domain.HookEvent{EventID: newID(), SessionID: id, HookEventName: name, ObservedAt: at, Kind: "code", Model: cliModel, Cwd: cliCwd, Origin: origin}
	if name == "StopFailure" {
		v.Error = "rate_limit_error"
	}
	if edit != nil {
		edit(&v)
	}
	if err := e.HandleEvent(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalSessionIsHostedThroughTerminalExecutor(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	terminalHook(t, e, cliA, "StopFailure", terminalOrigin(700, true, clock.at), clock.at, nil)
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	if s := state(t, e, cliA); s.Host == nil || s.Host.Executor != domain.ExecutorTerminal || s.Host.Problem != "" {
		t.Fatalf("terminal session not hosted: %+v", s.Host)
	}
	scan(t, e)
	if b.count() != 1 {
		t.Fatalf("terminal session not resumed, calls=%d", b.count())
	}
	got := b.bindings[0]
	if got.Executor != domain.ExecutorTerminal || got.TargetIdentifier != "claude-cli:"+cliA || got.WorkDir != cliCwd || got.Origin == nil || got.Origin.TmuxPane != "%1" || got.Origin.Detached {
		t.Fatalf("wrong terminal delivery %+v origin=%+v", got, got.Origin)
	}
}

func TestTerminalSessionWithoutChannelIsMonitorOnly(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	refresh(t, e, q, freshAccount("a", clock.at))
	noProcess := terminalOrigin(700, false, clock.at)
	noProcess.ClaudePID, noProcess.Ancestors, noProcess.ExecPath = 0, nil, ""
	terminalHook(t, e, cliA, "StopFailure", noProcess, clock.at, nil)
	noPath := terminalOrigin(800, false, clock.at)
	noPath.ExecPath = ""
	terminalHook(t, e, cliB, "StopFailure", noPath, clock.at, nil)
	for id, want := range map[string]string{cliA: reasonNoProcess, cliB: reasonNoTerminalPath} {
		s := state(t, e, id)
		if s.Host == nil || s.Host.Ready || s.Host.Executor != "" || s.Host.Problem != want {
			t.Fatalf("%s: %+v, want %q", id, s.Host, want)
		}
	}
	scan(t, e)
	if b.count() != 0 {
		t.Fatal("unroutable terminal session was dispatched")
	}
}

func TestTerminalExitKeepsQueuedContinueForHeadlessResume(t *testing.T) {
	e, q, b, clock := hostedFixture(t, true)
	origin := terminalOrigin(700, true, clock.at)
	terminalHook(t, e, cliA, "StopFailure", origin, clock.at, nil)
	clock.at = clock.at.Add(time.Second)
	terminalHook(t, e, cliA, "SessionEnd", origin, clock.at, func(v *domain.HookEvent) { v.Reason = "prompt_input_exit" })
	s := state(t, e, cliA)
	if s.State != domain.Cooldown || s.Origin == nil || !s.Origin.Detached {
		t.Fatalf("exit must keep the quota continue and detach the pane: state=%s origin=%+v", s.State, s.Origin)
	}
	pending := false
	for _, j := range jobsOf(t, e, cliA) {
		pending = pending || j.State == domain.Cooldown || j.State == domain.Ready
	}
	if !pending {
		t.Fatal("quota job was cancelled by a normal terminal exit")
	}
	clock.at = clock.at.Add(time.Second)
	refresh(t, e, q, freshAccount("a", clock.at))
	scan(t, e)
	if b.count() != 1 || b.bindings[0].Origin == nil || !b.bindings[0].Origin.Detached || b.bindings[0].WorkDir != cliCwd {
		t.Fatalf("closed session not resumed headlessly: calls=%d", b.count())
	}
	if state(t, e, cliA).State != domain.Verifying {
		t.Fatal("headless submission must wait for a hook")
	}
	// The headless process announces itself before its prompt hook.
	headless := terminalOrigin(900, false, clock.at.Add(time.Second))
	headless.TermProgram, headless.Entrypoint = "", "sdk-cli"
	terminalHook(t, e, cliA, "SessionStart", headless, clock.at.Add(time.Second), func(v *domain.HookEvent) { v.Source = "resume" })
	if state(t, e, cliA).State != domain.Verifying {
		t.Fatal("SessionStart(resume) must not abandon the dispatched job")
	}
	terminalHook(t, e, cliA, "UserPromptSubmit", headless, clock.at.Add(2*time.Second), nil)
	confirmed := false
	for _, j := range jobsOf(t, e, cliA) {
		confirmed = confirmed || (j.State == domain.Completed && j.AccountID == "a")
	}
	if !confirmed || state(t, e, cliA).State != domain.Running {
		t.Fatal("headless resume not confirmed by its prompt hook")
	}
}

func TestTerminalClearCancelsQueuedContinue(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	origin := terminalOrigin(700, true, clock.at)
	terminalHook(t, e, cliA, "StopFailure", origin, clock.at, nil)
	terminalHook(t, e, cliA, "SessionEnd", origin, clock.at.Add(time.Second), func(v *domain.HookEvent) { v.Reason = "clear" })
	if s := state(t, e, cliA); s.State != domain.Cancelled {
		t.Fatalf("/clear must cancel, got %s", s.State)
	}
	for _, j := range jobsOf(t, e, cliA) {
		if j.State == domain.Cooldown || j.State == domain.Ready {
			t.Fatal("job still pending after /clear")
		}
	}
}

func TestTerminalSiblingOnSameProcessIsDetached(t *testing.T) {
	e, _, _, clock := hostedFixture(t, true)
	terminalHook(t, e, cliA, "StopFailure", terminalOrigin(700, true, clock.at), clock.at, nil)
	terminalHook(t, e, "other", "Stop", terminalOrigin(800, true, clock.at), clock.at, nil)
	// /resume inside the same claude process now hosts session B.
	terminalHook(t, e, cliB, "SessionStart", terminalOrigin(700, true, clock.at.Add(time.Second)), clock.at.Add(time.Second), nil)
	if o := state(t, e, cliA).Origin; o == nil || !o.Detached {
		t.Fatalf("session A still bound to the process now hosting B: %+v", o)
	}
	if o := state(t, e, cliB).Origin; o == nil || o.Detached {
		t.Fatalf("current session detached: %+v", o)
	}
	if o := state(t, e, "other").Origin; o == nil || o.Detached {
		t.Fatalf("unrelated process detached: %+v", o)
	}
}
