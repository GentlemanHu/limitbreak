package executor

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/host"
)

// Terminal delivers continue messages to claude CLI sessions. A session whose
// process is still alive inside tmux receives the text through tmux send-keys
// into its own pane; a session whose process has ended is resumed headlessly
// with `claude -p --resume <id>`. A live session outside tmux is never typed
// into: it waits until it is closed. Nothing is ever resent after any input.
type Terminal struct {
	lookup func(int) (host.Process, bool)
	exe    func(int) string
	idle   func() (time.Duration, bool)
	stat   func(string) (fs.FileInfo, error)
	sleep  func(context.Context, time.Duration) error
	// tmux runs one tmux client command and returns its stdout.
	tmux func(ctx context.Context, binary string, args ...string) (string, error)
	// start launches a detached headless claude; Wait reports its exit.
	start func(binary string, args []string, dir string, env []string) (interface{ Wait() error }, error)
	uid   int
	env   []string

	mu      sync.Mutex
	running map[string]bool
}

func NewTerminal() *Terminal {
	return &Terminal{
		lookup: host.Lookup, exe: host.ExecutablePath, idle: host.UserIdle, stat: os.Stat,
		sleep: sleepContext, tmux: runTmux, start: startDetached,
		uid: os.Getuid(), env: headlessEnv(os.Environ()),
	}
}

func terminalResult(ok bool, code, message string) domain.BridgeResult {
	return domain.BridgeResult{OK: ok, Code: code, Message: message, AccessibilityGranted: true}
}

func terminalFailure(code, message string) *domain.BridgeResult {
	r := terminalResult(false, code, message)
	return &r
}

// terminalPlan is the channel chosen for one operation: a verified tmux pane,
// or the claude executable for a headless resume.
type terminalPlan struct {
	pane     *tmuxPane
	headless string
}

// Probe has no global prerequisite; every check is per session.
func (t *Terminal) Probe(context.Context) domain.BridgeResult {
	return terminalResult(true, "terminal_ready", "")
}

func (t *Terminal) Preflight(ctx context.Context, b domain.Binding) domain.BridgeResult {
	p, problem := t.route(ctx, b)
	if problem != nil {
		return *problem
	}
	if p.pane != nil {
		if problem := t.userIdle(b); problem != nil {
			return *problem
		}
		return terminalResult(true, "tmux_ready", "会话在 tmux 窗格 "+p.pane.id+" 中等待输入。")
	}
	return terminalResult(true, "headless_ready", "会话已关闭，将在后台以 claude -p --resume 续写。")
}

func (t *Terminal) ValidateTarget(ctx context.Context, b domain.Binding) domain.BridgeResult {
	p, problem := t.route(ctx, b)
	if problem != nil {
		return *problem
	}
	if p.pane != nil {
		return terminalResult(true, "tmux_pane_verified", "已核验 tmux 窗格 "+p.pane.id+" 正运行该会话的 claude。")
	}
	return terminalResult(true, "headless_verified", "会话已关闭；已核验 claude 可执行文件与工作目录。")
}

func (t *Terminal) Resume(ctx context.Context, b domain.Binding, message string) (domain.BridgeResult, error) {
	if !domain.ValidContinueMessage(message) {
		return *terminalFailure("message_invalid", "继续文案无效。"), nil
	}
	p, problem := t.route(ctx, b)
	if problem != nil {
		return *problem, nil
	}
	if p.pane != nil {
		if problem := t.userIdle(b); problem != nil {
			return *problem, nil
		}
		return t.sendTmux(ctx, *p.pane, message), nil
	}
	return t.resumeHeadless(b, p.headless, message), nil
}

func (t *Terminal) Locate(ctx context.Context, b domain.Binding) domain.BridgeResult {
	p, problem := t.route(ctx, b)
	if problem != nil {
		return *problem
	}
	if p.pane == nil {
		return terminalResult(false, "locate_unavailable", "该终端会话已关闭，没有可定位的窗口；额度恢复后会在后台续写。")
	}
	return t.selectPane(ctx, *p.pane)
}

// route decides between the live pane and a headless resume from kernel facts
// observed now, never from the recorded origin alone.
func (t *Terminal) route(ctx context.Context, b domain.Binding) (terminalPlan, *domain.BridgeResult) {
	o := b.Origin
	if b.Executor != domain.ExecutorTerminal || o == nil || o.App != domain.AppTerminal || !domain.ValidCLISessionID(b.SessionID) {
		return terminalPlan{}, terminalFailure("terminal_origin_invalid", "该会话不是可识别的终端 claude 会话。")
	}
	if !o.Detached {
		if o.ClaudePID <= 1 {
			return terminalPlan{}, terminalFailure("terminal_process_unknown", "未采集到该会话的 claude 进程，无法判断会话是否仍开着。")
		}
		// A recorded PID only counts while the same process (started before the
		// observation) still runs; a reused PID means the session has ended.
		if proc, alive := t.lookup(o.ClaudePID); alive && !proc.Start.After(o.ObservedAt) {
			if o.TmuxPane == "" {
				return terminalPlan{}, terminalFailure("terminal_session_open", "会话仍在终端里打开但不在 tmux 中，无法安全输入；关闭该会话后将改为后台续写。")
			}
			pane, problem := t.verifyPane(ctx, *o, proc)
			if problem != nil {
				return terminalPlan{}, problem
			}
			return terminalPlan{pane: &pane}, nil
		}
	}
	binary, problem := t.verifyHeadless(b)
	if problem != nil {
		return terminalPlan{}, problem
	}
	return terminalPlan{headless: binary}, nil
}

func (t *Terminal) userIdle(b domain.Binding) *domain.BridgeResult {
	need := time.Duration(b.ExecutionUserIdleSeconds) * time.Second
	if need <= 0 {
		need = 3 * time.Second
	}
	idle, ok := t.idle()
	if !ok {
		return terminalFailure("idle_state_unverified", "无法读取键鼠空闲时间，暂不向终端输入。")
	}
	if idle < need {
		return terminalFailure("user_active", "你正在使用键盘或鼠标，稍后再向终端输入。")
	}
	return nil
}

// verifyHeadless checks the executable and directory a headless resume would
// use. The binary must be a claude executable only the user or root can modify.
func (t *Terminal) verifyHeadless(b domain.Binding) (string, *domain.BridgeResult) {
	if !domain.ValidAbsolutePath(b.WorkDir) {
		return "", terminalFailure("terminal_workdir_invalid", "会话没有可用的工作目录，无法后台续写。")
	}
	if info, err := t.stat(b.WorkDir); err != nil || !info.IsDir() {
		return "", terminalFailure("terminal_workdir_invalid", "会话的工作目录已不存在，无法后台续写。")
	}
	binary := ""
	if b.Origin != nil {
		binary = b.Origin.ExecPath
	}
	if !domain.ValidAbsolutePath(binary) || filepath.Base(binary) != "claude" {
		return "", terminalFailure("terminal_binary_unknown", "未采集到 claude 可执行文件路径，无法后台续写。")
	}
	info, err := t.stat(binary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 || !ownedBy(info, t.uid) {
		return "", terminalFailure("terminal_binary_unverified", "claude 可执行文件不存在、不可执行或可被他人修改，拒绝后台续写。")
	}
	t.mu.Lock()
	busy := t.running[b.SessionID]
	t.mu.Unlock()
	if busy {
		return "", terminalFailure("terminal_busy", "该会话的后台续写仍在运行。")
	}
	return binary, nil
}

func ownedBy(info fs.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && (int(st.Uid) == uid || st.Uid == 0)
}

// resumeHeadless starts one detached `claude -p --resume`. The process start is
// the only side effect; submission is confirmed by the session's own hooks.
func (t *Terminal) resumeHeadless(b domain.Binding, binary, message string) domain.BridgeResult {
	if strings.HasPrefix(message, "-") {
		return *terminalFailure("message_unsupported", "后台续写不发送以“-”开头的文案，以免被当作命令行选项。")
	}
	t.mu.Lock()
	if t.running == nil {
		t.running = map[string]bool{}
	}
	if t.running[b.SessionID] {
		t.mu.Unlock()
		return *terminalFailure("terminal_busy", "该会话的后台续写仍在运行。")
	}
	t.running[b.SessionID] = true
	t.mu.Unlock()
	process, err := t.start(binary, []string{"-p", "--resume", b.SessionID, message}, b.WorkDir, t.env)
	if err != nil {
		t.mu.Lock()
		delete(t.running, b.SessionID)
		t.mu.Unlock()
		return *terminalFailure("headless_start_failed", "无法启动后台 claude 进程；未发送。")
	}
	go func() {
		_ = process.Wait()
		t.mu.Lock()
		delete(t.running, b.SessionID)
		t.mu.Unlock()
	}()
	r := terminalResult(true, "submitted", "已在后台以 claude -p --resume 续写该会话；等待会话 Hook 确认。")
	r.Submitted = true
	return r
}

func startDetached(binary string, args []string, dir string, env []string) (interface{ Wait() error }, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir, cmd.Env = dir, env
	// Own session: the run survives the guard and never shares a terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// headlessEnv keeps only what claude needs to find its configuration; tmux,
// terminal and hook-identity variables of the guard are not passed on.
func headlessEnv(environ []string) []string {
	keep := map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TMPDIR": true, "CLAUDE_CONFIG_DIR": true}
	var out []string
	hasPath := false
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && keep[name] {
			out = append(out, kv)
			hasPath = hasPath || name == "PATH"
		}
	}
	if !hasPath {
		out = append(out, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	}
	return out
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
