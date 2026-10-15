package executor

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"quota-session-orchestrator/internal/domain"
)

// ClientSpec is one configured generic client: an absolute executable plus
// arguments in which the placeholders below are replaced as whole arguments.
type ClientSpec struct {
	ID      string
	Command []string
	Env     []string
	Timeout time.Duration
}

const (
	argSessionID = "{session_id}"
	argCwd       = "{cwd}"
	argMessage   = "{message}"
)

// Command continues a generic client session by running its configured
// command without a shell. Starting the process is the only side effect; like
// the headless terminal resume, submission is confirmed only by the client's
// own turn_started/turn_finished event. The process is killed after Timeout.
type Command struct {
	spec  ClientSpec
	stat  func(string) (fs.FileInfo, error)
	start func(binary string, args []string, dir string, env []string, timeout time.Duration) (interface{ Wait() error }, error)
	uid   int
	env   []string

	mu      sync.Mutex
	running map[string]bool
}

func NewCommand(spec ClientSpec) *Command {
	if spec.Timeout <= 0 {
		spec.Timeout = 120 * time.Second
	}
	return &Command{spec: spec, stat: os.Stat, start: startBounded, uid: os.Getuid(), env: clientEnv(os.Environ(), spec.Env)}
}

func commandFailure(code, message string) domain.BridgeResult {
	return terminalResult(false, code, message)
}

func (c *Command) Probe(context.Context) domain.BridgeResult {
	if problem := c.verifyBinary(); problem != nil {
		return *problem
	}
	return terminalResult(true, "client_ready", "")
}

func (c *Command) Preflight(_ context.Context, b domain.Binding) domain.BridgeResult {
	if problem := c.check(b); problem != nil {
		return *problem
	}
	return terminalResult(true, "client_ready", "将运行客户端 "+c.spec.ID+" 配置的命令继续该会话。")
}

func (c *Command) ValidateTarget(_ context.Context, b domain.Binding) domain.BridgeResult {
	if problem := c.check(b); problem != nil {
		return *problem
	}
	return terminalResult(true, "client_verified", "已核验客户端 "+c.spec.ID+" 的命令可执行。")
}

func (c *Command) Locate(context.Context, domain.Binding) domain.BridgeResult {
	return commandFailure("locate_unavailable", "通用客户端会话没有可定位的窗口。")
}

func (c *Command) Resume(_ context.Context, b domain.Binding, message string) (domain.BridgeResult, error) {
	if !domain.ValidContinueMessage(message) {
		return commandFailure("message_invalid", "继续文案无效。"), nil
	}
	if strings.HasPrefix(message, "-") {
		return commandFailure("message_unsupported", "客户端命令不发送以“-”开头的文案，以免被当作命令行选项。"), nil
	}
	if problem := c.check(b); problem != nil {
		return *problem, nil
	}
	c.mu.Lock()
	if c.running == nil {
		c.running = map[string]bool{}
	}
	if c.running[b.SessionID] {
		c.mu.Unlock()
		return commandFailure("session_busy", "该会话的客户端命令仍在运行。"), nil
	}
	c.running[b.SessionID] = true
	c.mu.Unlock()
	dir := c.workDir(b)
	args := c.args(b, dir, message)
	env := append(append([]string{}, c.env...), "LIMITBREAK_CLIENT_ID="+c.spec.ID, "LIMITBREAK_SESSION_ID="+b.SessionID)
	process, err := c.start(c.spec.Command[0], args, dir, env, c.spec.Timeout)
	if err != nil {
		c.mu.Lock()
		delete(c.running, b.SessionID)
		c.mu.Unlock()
		return commandFailure("client_start_failed", "无法启动客户端命令；未发送。"), nil
	}
	go func() {
		_ = process.Wait()
		c.mu.Lock()
		delete(c.running, b.SessionID)
		c.mu.Unlock()
	}()
	r := terminalResult(true, "client_submitted", "已运行客户端 "+c.spec.ID+" 的继续命令；等待客户端事件确认。")
	r.Submitted = true
	return r, nil
}

// check verifies the binding belongs to this client and the executable is a
// regular file only the user or root can modify.
func (c *Command) check(b domain.Binding) *domain.BridgeResult {
	if b.Executor != domain.ClientExecutor(c.spec.ID) || !domain.ValidClientSessionID(b.SessionID) || b.Origin == nil || b.Origin.App != domain.AppClient || b.Origin.Client != c.spec.ID {
		r := commandFailure("client_origin_invalid", "该会话不属于客户端 "+c.spec.ID+"。")
		return &r
	}
	return c.verifyBinary()
}

func (c *Command) verifyBinary() *domain.BridgeResult {
	if len(c.spec.Command) == 0 || !domain.ValidAbsolutePath(c.spec.Command[0]) {
		r := commandFailure("client_unconfigured", "客户端 "+c.spec.ID+" 未配置可执行文件。")
		return &r
	}
	info, err := c.stat(c.spec.Command[0])
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 || !ownedBy(info, c.uid) {
		r := commandFailure("client_unavailable", "客户端 "+c.spec.ID+" 的可执行文件不存在、不可执行或可被他人修改。")
		return &r
	}
	return nil
}

func (c *Command) workDir(b domain.Binding) string {
	if domain.ValidAbsolutePath(b.WorkDir) {
		if info, err := c.stat(b.WorkDir); err == nil && info.IsDir() {
			return b.WorkDir
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "/"
}

// args substitutes whole-argument placeholders; values never pass a shell.
func (c *Command) args(b domain.Binding, dir, message string) []string {
	out := make([]string, 0, len(c.spec.Command)-1)
	for _, arg := range c.spec.Command[1:] {
		switch arg {
		case argSessionID:
			arg = b.SessionID
		case argCwd:
			arg = dir
		case argMessage:
			arg = message
		}
		out = append(out, arg)
	}
	return out
}

type boundedProcess struct {
	cmd   *exec.Cmd
	timer *time.Timer
}

func (p boundedProcess) Wait() error {
	err := p.cmd.Wait()
	p.timer.Stop()
	return err
}

// startBounded starts the command in its own process group, discards its
// output and kills the group once the timeout passes.
func startBounded(binary string, args []string, dir string, env []string, timeout time.Duration) (interface{ Wait() error }, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	timer := time.AfterFunc(timeout, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return boundedProcess{cmd: cmd, timer: timer}, nil
}

// clientEnv keeps the basic variables plus the client's allowlist.
func clientEnv(environ []string, allow []string) []string {
	keep := map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TMPDIR": true}
	for _, name := range allow {
		keep[name] = true
	}
	var out []string
	hasPath := false
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && keep[name] && !strings.HasPrefix(name, "LIMITBREAK_") {
			out = append(out, kv)
			hasPath = hasPath || name == "PATH"
		}
	}
	if !hasPath {
		out = append(out, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	}
	return out
}
