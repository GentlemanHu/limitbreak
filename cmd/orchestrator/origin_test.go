package main

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func envOf(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func chainOf(parents map[int]int) func(int) int {
	return func(pid int) int {
		if p, ok := parents[pid]; ok {
			return p
		}
		return -1
	}
}

func noExe(int) string { return "" }

func exeOf(paths map[int]string) func(int) string {
	return func(pid int) string { return paths[pid] }
}

func TestCaptureOriginDesktop(t *testing.T) {
	now := time.Now()
	o := captureOrigin(envOf(map[string]string{
		"CLAUDE_CODE_ENTRYPOINT":      "claude-desktop-3p",
		"__CFBundleIdentifier":        domain.DesktopBundleID,
		"CLAUDE_CODE_HOST_SESSION_ID": "local_0a1b2c3d-1111-4222-8333-444455556666",
		"CLAUDE_PID":                  "4242",
		"CLAUDE_CODE_EXECPATH":        "/Users/x/Library/Application Support/Claude/claude-code/2.1.267/claude",
	}), 500, chainOf(map[int]int{500: 400, 400: 300, 300: 1}), noExe, now)
	if o == nil || o.App != domain.AppClaudeDesktop || o.ClaudePID != 4242 || o.EngineVersion != "2.1.267" || !reflect.DeepEqual(o.Ancestors, []int{500, 400, 300}) {
		t.Fatalf("unexpected origin %+v", o)
	}
	if domain.DesktopDeepLink(o.HostSessionID) == "" {
		t.Fatal("host session lost")
	}
}

func TestCaptureOriginTerminalAndHostileInput(t *testing.T) {
	cycle := chainOf(map[int]int{10: 11, 11: 10})
	o := captureOrigin(envOf(map[string]string{
		"TERM_PROGRAM":                "iTerm.app",
		"CLAUDE_CODE_HOST_SESSION_ID": "../../etc",
		"CLAUDE_PID":                  "-3",
		"CLAUDE_CODE_ENTRYPOINT":      "cli\r\ninjected" + strings.Repeat("界", 80),
	}), 10, cycle, noExe, time.Now())
	if o == nil || o.App != domain.AppTerminal || o.HostSessionID != "" || o.ClaudePID != 0 || !reflect.DeepEqual(o.Ancestors, []int{10, 11}) {
		t.Fatalf("unexpected origin %+v", o)
	}
	if len(o.Entrypoint) > 128 || strings.ContainsAny(o.Entrypoint, "\r\n") {
		t.Fatalf("field not clipped: %q", o.Entrypoint)
	}
	deep := map[int]int{}
	for pid := 2; pid < 40; pid++ {
		deep[pid] = pid + 1
	}
	if o := captureOrigin(envOf(nil), 2, chainOf(deep), noExe, time.Now()); o == nil || len(o.Ancestors) != maxAncestors || o.App != domain.AppUnknown {
		t.Fatalf("chain not bounded: %+v", o)
	}
}

func TestCaptureOriginTmuxAndExecutable(t *testing.T) {
	chain := chainOf(map[int]int{700: 600, 600: 500, 500: 1})
	exe := exeOf(map[int]string{600: "/Users/x/.local/share/claude/versions/2.1.300/claude", 500: "/opt/homebrew/bin/tmux"})
	o := captureOrigin(envOf(map[string]string{
		"CLAUDE_CODE_ENTRYPOINT": "cli",
		"TMUX":                   "/private/tmp/tmux-501/default,500,0",
		"TMUX_PANE":              "%7",
	}), 700, chain, exe, time.Now())
	if o == nil || o.App != domain.AppTerminal || o.ClaudePID != 600 || o.ExecPath != "/Users/x/.local/share/claude/versions/2.1.300/claude" {
		t.Fatalf("claude process not derived from the chain: %+v", o)
	}
	if o.TmuxSocket != "/private/tmp/tmux-501/default" || o.TmuxServerPID != 500 || o.TmuxPane != "%7" {
		t.Fatalf("tmux facts lost: %+v", o)
	}
	// Malformed or partial tmux facts are dropped together, never half-kept.
	for _, env := range []map[string]string{
		{"TMUX": "relative/sock,500,0", "TMUX_PANE": "%7"},
		{"TMUX": "/tmp/s,1,0", "TMUX_PANE": "%7"},
		{"TMUX": "/tmp/s,500,0", "TMUX_PANE": "7; rm"},
		{"TMUX_PANE": "%7"},
	} {
		env["TERM_PROGRAM"] = "tmux"
		env["CLAUDE_PID"] = "600"
		env["CLAUDE_CODE_EXECPATH"] = "/usr/local/bin/node"
		o := captureOrigin(envOf(env), 700, chain, noExe, time.Now())
		if o == nil || o.TmuxPane != "" || o.TmuxSocket != "" || o.TmuxServerPID != 0 || o.ExecPath != "" || o.ClaudePID != 600 {
			t.Fatalf("partial tmux or foreign executable accepted for %v: %+v", env, o)
		}
	}
}

func TestHookInputCannotForgeOrigin(t *testing.T) {
	raw := []byte(`{"session_id":"s","hook_event_name":"Stop","origin":{"app":"claude-desktop","host_session_id":"local_0a1b2c3d-1111-4222-8333-444455556666"}}`)
	e, err := normalizeHook(raw, time.Now())
	if err != nil || e.Origin != nil {
		t.Fatalf("hook input supplied an origin: %+v %v", e.Origin, err)
	}
}

func TestParentPIDOfSelf(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("kernel process table lookup is macOS-only")
	}
	if got := parentPID(os.Getpid()); got != os.Getppid() {
		t.Fatalf("parentPID=%d want %d", got, os.Getppid())
	}
}
