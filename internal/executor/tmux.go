package executor

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/host"
)

// tmuxPane is a pane verified to host the session's live claude process.
type tmuxPane struct {
	binary string
	socket string
	id     string
}

// verifyPane proves that the recorded pane still hosts the recorded claude:
// the tmux server is the same process, its socket belongs to the user, the
// pane's shell is a live ancestor of claude, and claude owns the terminal.
func (t *Terminal) verifyPane(ctx context.Context, o domain.SessionOrigin, claude host.Process) (tmuxPane, *domain.BridgeResult) {
	if !claude.Foreground() {
		return tmuxPane{}, terminalFailure("terminal_not_foreground", "claude 当前不是该终端的前台进程（可能被挂起或正在运行子命令），暂不输入。")
	}
	server, ok := t.lookup(o.TmuxServerPID)
	if !ok || server.Start.After(o.ObservedAt) {
		return tmuxPane{}, terminalFailure("tmux_server_changed", "记录的 tmux 服务已不存在或已重启，拒绝输入。")
	}
	binary := t.exe(o.TmuxServerPID)
	if !domain.ValidAbsolutePath(binary) || filepath.Base(binary) != "tmux" {
		return tmuxPane{}, terminalFailure("tmux_binary_unverified", "无法确认 tmux 服务对应的可执行文件，拒绝输入。")
	}
	info, err := t.stat(o.TmuxSocket)
	if err != nil || info.Mode()&fs.ModeSocket == 0 || !ownedBy(info, t.uid) {
		return tmuxPane{}, terminalFailure("tmux_socket_unverified", "tmux 套接字不存在或不属于当前用户，拒绝输入。")
	}
	pane := tmuxPane{binary: binary, socket: o.TmuxSocket, id: o.TmuxPane}
	out, err := t.tmux(ctx, binary, "-S", pane.socket, "display-message", "-p", "-t", pane.id, "#{pane_pid} #{pane_dead}")
	fields := strings.Fields(out)
	if err != nil || len(fields) != 2 || fields[1] != "0" {
		return tmuxPane{}, terminalFailure("tmux_pane_missing", "tmux 窗格 "+pane.id+" 已不存在，拒绝输入。")
	}
	shell, err := strconv.Atoi(fields[0])
	if err != nil || shell <= 1 || !contains(o.Ancestors, shell) || !t.liveAncestor(claude.PID, shell) {
		return tmuxPane{}, terminalFailure("tmux_pane_mismatch", "tmux 窗格 "+pane.id+" 中运行的已不是该会话的 claude，拒绝输入。")
	}
	return pane, nil
}

// liveAncestor walks the current parent chain of pid looking for ancestor.
func (t *Terminal) liveAncestor(pid, ancestor int) bool {
	for i := 0; i < 16 && pid > 1; i++ {
		p, ok := t.lookup(pid)
		if !ok {
			return false
		}
		if p.PPID == ancestor {
			return true
		}
		pid = p.PPID
	}
	return false
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// sendTmux types the message literally, then presses Enter once. A failure
// after the first command is uncertain and is never retried or cleared.
func (t *Terminal) sendTmux(ctx context.Context, pane tmuxPane, message string) domain.BridgeResult {
	// tmux treats an argument ending in ";" as a command separator.
	if strings.HasSuffix(message, ";") {
		return *terminalFailure("message_unsupported", "tmux 无法原样输入以分号结尾的文案，请修改继续文案。")
	}
	if _, err := t.tmux(ctx, pane.binary, "-S", pane.socket, "send-keys", "-t", pane.id, "-l", "--", message); err != nil {
		return terminalResult(false, "submitted_uncertain", "向 tmux 窗格输入时出错，无法确认是否已输入；不会重试，请到终端确认。")
	}
	// Separate the text from Enter so the CLI does not treat it as one paste.
	if err := t.sleep(ctx, 300*time.Millisecond); err != nil {
		return terminalResult(false, "submitted_uncertain", "文案已输入但未回车；不会重试，请到终端确认。")
	}
	if _, err := t.tmux(ctx, pane.binary, "-S", pane.socket, "send-keys", "-t", pane.id, "Enter"); err != nil {
		return terminalResult(false, "submitted_uncertain", "文案已输入但回车结果不确定；不会重试，请到终端确认。")
	}
	r := terminalResult(true, "submitted", "已在 tmux 窗格 "+pane.id+" 输入并回车；等待会话 Hook 确认。")
	r.Submitted = true
	return r
}

// selectPane switches tmux to the pane; the terminal app itself is not raised.
func (t *Terminal) selectPane(ctx context.Context, pane tmuxPane) domain.BridgeResult {
	for _, command := range []string{"select-window", "select-pane"} {
		if _, err := t.tmux(ctx, pane.binary, "-S", pane.socket, command, "-t", pane.id); err != nil {
			return terminalResult(false, "locate_failed", "tmux 切换到窗格 "+pane.id+" 失败。")
		}
	}
	return terminalResult(true, "located", "已在 tmux 中切到窗格 "+pane.id+"；终端窗口本身不会被带到前台。")
}

// runTmux runs one bounded tmux client command with a minimal environment.
func runTmux(ctx context.Context, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_CTYPE=UTF-8", "HOME=" + os.Getenv("HOME")}
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{buffer: &out, left: 4096}
	err := cmd.Run()
	return out.String(), err
}

type limitedWriter struct {
	buffer *bytes.Buffer
	left   int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > w.left {
		p = p[:w.left]
	}
	w.buffer.Write(p)
	w.left -= len(p)
	return n, nil
}
