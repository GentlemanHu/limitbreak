package main

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"quota-session-orchestrator/internal/domain"
)

// maxAncestors bounds the process chain walk; domain.ValidateOrigin allows 16.
const maxAncestors = 12

var engineVersionPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// captureOrigin records only what the hook process environment and the kernel
// process table prove about the session host. Window titles are never used.
// exe reads the executable a pid runs, or "" when unknown.
func captureOrigin(env func(string) string, parent int, ppid func(int) int, exe func(int) string, now time.Time) *domain.SessionOrigin {
	o := domain.SessionOrigin{
		Entrypoint:  clipField(env("CLAUDE_CODE_ENTRYPOINT")),
		TermProgram: clipField(env("TERM_PROGRAM")),
		BundleID:    clipField(env("__CFBundleIdentifier")),
		ObservedAt:  now.UTC(),
	}
	if id := env("CLAUDE_CODE_HOST_SESSION_ID"); domain.ValidHostSessionID(id) {
		o.HostSessionID = id
	}
	if pid, err := strconv.Atoi(env("CLAUDE_PID")); err == nil && pid > 1 {
		o.ClaudePID = pid
	}
	execPath := env("CLAUDE_CODE_EXECPATH")
	if versions := engineVersionPattern.FindAllString(execPath, -1); len(versions) > 0 {
		o.EngineVersion = versions[len(versions)-1]
	}
	o.Ancestors = ancestors(parent, ppid)
	if o.ClaudePID == 0 {
		// Without CLAUDE_PID, the nearest ancestor running a claude binary is the session process.
		for _, pid := range o.Ancestors {
			if claudeBinary(exe(pid)) {
				o.ClaudePID = pid
				break
			}
		}
	}
	if claudeBinary(execPath) {
		o.ExecPath = execPath
	} else if p := exe(o.ClaudePID); o.ClaudePID > 1 && claudeBinary(p) {
		o.ExecPath = p
	}
	if pane := env("TMUX_PANE"); domain.ValidTmuxPane(pane) {
		if socket, server, ok := parseTmux(env("TMUX")); ok {
			o.TmuxSocket, o.TmuxServerPID, o.TmuxPane = socket, server, pane
		}
	}
	o.App = domain.ClassifyOrigin(o)
	if domain.ValidateOrigin(o) != nil {
		return nil
	}
	return &o
}

// claudeBinary accepts an absolute path to an executable named claude.
func claudeBinary(p string) bool {
	return domain.ValidAbsolutePath(p) && filepath.Base(p) == "claude"
}

// parseTmux reads TMUX="socket_path,server_pid,session_index".
func parseTmux(v string) (string, int, bool) {
	parts := strings.Split(v, ",")
	if len(parts) != 3 || !domain.ValidAbsolutePath(parts[0]) {
		return "", 0, false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 1 {
		return "", 0, false
	}
	return parts[0], pid, true
}

// ancestors walks the parent chain from the hook's parent upwards, stopping at
// launchd, a lookup failure or a cycle.
func ancestors(pid int, ppid func(int) int) []int {
	var chain []int
	seen := map[int]bool{}
	for pid > 1 && len(chain) < maxAncestors && !seen[pid] {
		seen[pid] = true
		chain = append(chain, pid)
		pid = ppid(pid)
	}
	return chain
}

// clipField keeps a single-line UTF-8 prefix within the origin field limit.
func clipField(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	for len(s) > 128 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}
