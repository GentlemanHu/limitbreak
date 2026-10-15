package executor

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/host"
)

const (
	cliSession = "0b0d9c1e-3f43-4c39-9a35-6d1f6a0f7c11"
	claudeBin  = "/Users/u/.local/bin/claude"
	tmuxBin    = "/opt/homebrew/bin/tmux"
	tmuxSock   = "/private/tmp/tmux-501/default"
	workDir    = "/Users/u/project"
)

type fakeInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

type fakeWait struct{ done chan struct{} }

func (w fakeWait) Wait() error { <-w.done; return nil }

// terminalFixture is a host with claude 600 under pane shell 500 in tmux
// server 400; every fact can be changed per test.
type terminalFixture struct {
	t        *Terminal
	procs    map[int]host.Process
	exes     map[int]string
	files    map[string]fakeInfo
	idle     time.Duration
	idleOK   bool
	pane     string
	tmuxErr  map[string]error
	mu       sync.Mutex
	commands [][]string
	starts   [][]string
	dirs     []string
	release  chan struct{}
}

func newTerminalFixture(observed time.Time) *terminalFixture {
	before := observed.Add(-time.Hour)
	f := &terminalFixture{
		procs: map[int]host.Process{
			400: {PID: 400, PPID: 1, Start: before},
			500: {PID: 500, PPID: 400, Start: before},
			600: {PID: 600, PPID: 500, PGID: 600, TPGID: 600, Start: before},
		},
		exes: map[int]string{400: tmuxBin, 600: claudeBin},
		files: map[string]fakeInfo{
			tmuxSock:  {mode: fs.ModeSocket | 0o600, uid: 501},
			claudeBin: {mode: 0o755, uid: 501},
			workDir:   {mode: fs.ModeDir | 0o755, uid: 501},
		},
		idle: time.Minute, idleOK: true, pane: "500 0",
		tmuxErr: map[string]error{}, release: make(chan struct{}),
	}
	f.t = &Terminal{
		lookup: func(pid int) (host.Process, bool) { p, ok := f.procs[pid]; return p, ok },
		exe:    func(pid int) string { return f.exes[pid] },
		idle:   func() (time.Duration, bool) { return f.idle, f.idleOK },
		stat: func(p string) (fs.FileInfo, error) {
			if v, ok := f.files[p]; ok {
				return v, nil
			}
			return nil, fs.ErrNotExist
		},
		sleep: func(context.Context, time.Duration) error { return nil },
		tmux: func(_ context.Context, binary string, args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.commands = append(f.commands, append([]string{binary}, args...))
			command := args[2]
			if command == "send-keys" && args[len(args)-1] == "Enter" {
				command = "enter"
			}
			if err := f.tmuxErr[command]; err != nil {
				return "", err
			}
			if command == "display-message" {
				return f.pane + "\n", nil
			}
			return "", nil
		},
		start: func(binary string, args []string, dir string, env []string) (interface{ Wait() error }, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.starts = append(f.starts, append([]string{binary}, args...))
			f.dirs = append(f.dirs, dir)
			return fakeWait{done: f.release}, nil
		},
		uid: 501, env: []string{"HOME=/Users/u", "PATH=/usr/bin:/bin"},
	}
	return f
}

func terminalBinding(observed time.Time, tmux bool) domain.Binding {
	o := &domain.SessionOrigin{App: domain.AppTerminal, Entrypoint: "cli", ClaudePID: 600, Ancestors: []int{600, 500, 400}, ExecPath: claudeBin, ObservedAt: observed}
	if tmux {
		o.TmuxSocket, o.TmuxServerPID, o.TmuxPane = tmuxSock, 400, "%3"
	}
	return domain.Binding{SessionID: cliSession, Executor: domain.ExecutorTerminal, Origin: o, WorkDir: workDir, TargetIdentifier: "claude-cli:" + cliSession}
}

func (f *terminalFixture) tmuxCommands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		out = append(out, strings.Join(c[3:], " "))
	}
	return out
}

func TestTerminalTmuxSendsLiterallyThenEnter(t *testing.T) {
	now := time.Now()
	f := newTerminalFixture(now)
	b := terminalBinding(now, true)
	if r := f.t.Preflight(context.Background(), b); !r.OK || r.Code != "tmux_ready" || !r.AccessibilityGranted {
		t.Fatalf("preflight %+v", r)
	}
	f.commands = nil
	r, err := f.t.Resume(context.Background(), b, "继续 go on")
	if err != nil || !r.OK || !r.Submitted || r.Code != "submitted" {
		t.Fatalf("resume %+v %v", r, err)
	}
	want := []string{
		"display-message -p -t %3 #{pane_pid} #{pane_dead}",
		"send-keys -t %3 -l -- 继续 go on",
		"send-keys -t %3 Enter",
	}
	if got := f.tmuxCommands(); !reflect.DeepEqual(got, want) {
		t.Fatalf("tmux commands %q", got)
	}
	for _, c := range f.commands {
		if c[0] != tmuxBin || c[1] != "-S" || c[2] != tmuxSock {
			t.Fatalf("wrong tmux client or socket %q", c)
		}
	}
	if len(f.starts) != 0 {
		t.Fatal("live tmux session must not start a headless run")
	}
}

func TestTerminalTmuxRefusals(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		mutate  func(*terminalFixture, *domain.Binding)
		message string
		code    string
	}{
		{"user active", func(f *terminalFixture, _ *domain.Binding) { f.idle = time.Second }, "go", "user_active"},
		{"idle unknown", func(f *terminalFixture, _ *domain.Binding) { f.idleOK = false }, "go", "idle_state_unverified"},
		{"not foreground", func(f *terminalFixture, _ *domain.Binding) {
			p := f.procs[600]
			p.TPGID = 700
			f.procs[600] = p
		}, "go", "terminal_not_foreground"},
		{"server restarted", func(f *terminalFixture, _ *domain.Binding) {
			p := f.procs[400]
			p.Start = now.Add(time.Minute)
			f.procs[400] = p
		}, "go", "tmux_server_changed"},
		{"server binary", func(f *terminalFixture, _ *domain.Binding) { f.exes[400] = "/usr/bin/screen" }, "go", "tmux_binary_unverified"},
		{"foreign socket", func(f *terminalFixture, _ *domain.Binding) {
			f.files[tmuxSock] = fakeInfo{mode: fs.ModeSocket, uid: 777}
		}, "go", "tmux_socket_unverified"},
		{"socket not socket", func(f *terminalFixture, _ *domain.Binding) { f.files[tmuxSock] = fakeInfo{mode: 0o600, uid: 501} }, "go", "tmux_socket_unverified"},
		{"pane dead", func(f *terminalFixture, _ *domain.Binding) { f.pane = "500 1" }, "go", "tmux_pane_missing"},
		{"pane gone", func(f *terminalFixture, _ *domain.Binding) { f.tmuxErr["display-message"] = errors.New("no pane") }, "go", "tmux_pane_missing"},
		{"pane hosts another process", func(f *terminalFixture, _ *domain.Binding) { f.pane = "900 0" }, "go", "tmux_pane_mismatch"},
		{"recorded but no longer ancestor", func(f *terminalFixture, _ *domain.Binding) {
			p := f.procs[600]
			p.PPID = 400
			f.procs[600] = p
		}, "go", "tmux_pane_mismatch"},
		{"trailing semicolon", func(*terminalFixture, *domain.Binding) {}, "go;", "message_unsupported"},
		{"open outside tmux", func(_ *terminalFixture, b *domain.Binding) {
			b.Origin.TmuxSocket, b.Origin.TmuxServerPID, b.Origin.TmuxPane = "", 0, ""
		}, "go", "terminal_session_open"},
		{"invalid session id", func(_ *terminalFixture, b *domain.Binding) { b.SessionID = "term" }, "go", "terminal_origin_invalid"},
		{"desktop executor", func(_ *terminalFixture, b *domain.Binding) { b.Executor = domain.ExecutorDesktop }, "go", "terminal_origin_invalid"},
		{"process unknown", func(_ *terminalFixture, b *domain.Binding) { b.Origin.ClaudePID = 0 }, "go", "terminal_process_unknown"},
		{"invalid message", func(*terminalFixture, *domain.Binding) {}, "a\nb", "message_invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTerminalFixture(now)
			b := terminalBinding(now, true)
			c.mutate(f, &b)
			r, err := f.t.Resume(context.Background(), b, c.message)
			if err != nil || r.OK || r.Submitted || r.Code != c.code {
				t.Fatalf("got %+v %v, want %s", r, err, c.code)
			}
			for _, cmd := range f.tmuxCommands() {
				if strings.HasPrefix(cmd, "send-keys") {
					t.Fatalf("refusal typed into the pane: %q", cmd)
				}
			}
			if len(f.starts) != 0 {
				t.Fatal("refusal started a headless run")
			}
		})
	}
}

func TestTerminalTmuxFailuresAreUncertainAndNeverRetried(t *testing.T) {
	now := time.Now()
	for _, failing := range []string{"send-keys", "enter"} {
		f := newTerminalFixture(now)
		f.tmuxErr[failing] = errors.New("boom")
		r, _ := f.t.Resume(context.Background(), terminalBinding(now, true), "go")
		if r.OK || r.Code != "submitted_uncertain" {
			t.Fatalf("%s: %+v", failing, r)
		}
		sends := 0
		for _, cmd := range f.tmuxCommands() {
			if strings.HasPrefix(cmd, "send-keys") {
				sends++
			}
		}
		if want := map[string]int{"send-keys": 1, "enter": 2}[failing]; sends != want {
			t.Fatalf("%s: %d send-keys calls, want %d", failing, sends, want)
		}
	}
}

func TestTerminalLocateSelectsPane(t *testing.T) {
	now := time.Now()
	f := newTerminalFixture(now)
	if r := f.t.Locate(context.Background(), terminalBinding(now, true)); !r.OK || r.Code != "located" {
		t.Fatalf("locate %+v", r)
	}
	got := f.tmuxCommands()
	if len(got) != 3 || got[1] != "select-window -t %3" || got[2] != "select-pane -t %3" {
		t.Fatalf("locate commands %q", got)
	}
	f = newTerminalFixture(now)
	b := terminalBinding(now, true)
	b.Origin.Detached = true
	if r := f.t.Locate(context.Background(), b); r.OK || r.Code != "locate_unavailable" {
		t.Fatalf("closed session locate %+v", r)
	}
}

func TestTerminalHeadlessResume(t *testing.T) {
	now := time.Now()
	for _, name := range []string{"exited", "pid reused", "detached"} {
		t.Run(name, func(t *testing.T) {
			f := newTerminalFixture(now)
			b := terminalBinding(now, true)
			switch name {
			case "exited":
				delete(f.procs, 600)
			case "pid reused":
				p := f.procs[600]
				p.Start = now.Add(time.Minute)
				f.procs[600] = p
			case "detached":
				b.Origin.Detached = true
			}
			if r := f.t.Preflight(context.Background(), b); !r.OK || r.Code != "headless_ready" {
				t.Fatalf("preflight %+v", r)
			}
			f.idleOK = false // the idle gate only guards typing into a pane
			r, err := f.t.Resume(context.Background(), b, "继续")
			if err != nil || !r.OK || !r.Submitted || r.Code != "submitted" {
				t.Fatalf("resume %+v %v", r, err)
			}
			want := []string{claudeBin, "-p", "--resume", cliSession, "继续"}
			if len(f.starts) != 1 || !reflect.DeepEqual(f.starts[0], want) || f.dirs[0] != workDir {
				t.Fatalf("start %q in %q", f.starts, f.dirs)
			}
			for _, cmd := range f.tmuxCommands() {
				if strings.HasPrefix(cmd, "send-keys") {
					t.Fatalf("headless path typed into tmux: %q", cmd)
				}
			}
			if r, _ := f.t.Resume(context.Background(), b, "继续"); r.OK || r.Code != "terminal_busy" {
				t.Fatalf("second run while first is running: %+v", r)
			}
			close(f.release)
			deadline := time.Now().Add(2 * time.Second)
			for {
				f.t.mu.Lock()
				busy := f.t.running[cliSession]
				f.t.mu.Unlock()
				if !busy {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("running mark not cleared after exit")
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestTerminalHeadlessRefusals(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		mutate  func(*terminalFixture, *domain.Binding)
		message string
		code    string
	}{
		{"leading dash", func(*terminalFixture, *domain.Binding) {}, "--dangerously-skip-permissions", "message_unsupported"},
		{"no workdir", func(_ *terminalFixture, b *domain.Binding) { b.WorkDir = "" }, "go", "terminal_workdir_invalid"},
		{"workdir gone", func(f *terminalFixture, _ *domain.Binding) { delete(f.files, workDir) }, "go", "terminal_workdir_invalid"},
		{"workdir is file", func(f *terminalFixture, _ *domain.Binding) { f.files[workDir] = fakeInfo{mode: 0o644, uid: 501} }, "go", "terminal_workdir_invalid"},
		{"no binary", func(_ *terminalFixture, b *domain.Binding) { b.Origin.ExecPath = "" }, "go", "terminal_binary_unknown"},
		{"node binary", func(_ *terminalFixture, b *domain.Binding) { b.Origin.ExecPath = "/usr/local/bin/node" }, "go", "terminal_binary_unknown"},
		{"binary missing", func(f *terminalFixture, _ *domain.Binding) { delete(f.files, claudeBin) }, "go", "terminal_binary_unverified"},
		{"group writable", func(f *terminalFixture, _ *domain.Binding) { f.files[claudeBin] = fakeInfo{mode: 0o775, uid: 501} }, "go", "terminal_binary_unverified"},
		{"world writable", func(f *terminalFixture, _ *domain.Binding) { f.files[claudeBin] = fakeInfo{mode: 0o757, uid: 501} }, "go", "terminal_binary_unverified"},
		{"not executable", func(f *terminalFixture, _ *domain.Binding) { f.files[claudeBin] = fakeInfo{mode: 0o644, uid: 501} }, "go", "terminal_binary_unverified"},
		{"foreign owner", func(f *terminalFixture, _ *domain.Binding) { f.files[claudeBin] = fakeInfo{mode: 0o755, uid: 777} }, "go", "terminal_binary_unverified"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newTerminalFixture(now)
			b := terminalBinding(now, false)
			b.Origin.Detached = true
			c.mutate(f, &b)
			r, err := f.t.Resume(context.Background(), b, c.message)
			if err != nil || r.OK || r.Submitted || r.Code != c.code {
				t.Fatalf("got %+v %v, want %s", r, err, c.code)
			}
			if len(f.starts) != 0 {
				t.Fatal("refusal started a headless run")
			}
		})
	}
	f := newTerminalFixture(now)
	f.files[claudeBin] = fakeInfo{mode: 0o755, uid: 0}
	b := terminalBinding(now, false)
	b.Origin.Detached = true
	if r := f.t.ValidateTarget(context.Background(), b); !r.OK || r.Code != "headless_verified" {
		t.Fatalf("root-owned binary must be accepted: %+v", r)
	}
}

func TestTerminalHeadlessStartFailureIsNotSent(t *testing.T) {
	now := time.Now()
	f := newTerminalFixture(now)
	f.t.start = func(string, []string, string, []string) (interface{ Wait() error }, error) {
		return nil, errors.New("exec format error")
	}
	b := terminalBinding(now, false)
	b.Origin.Detached = true
	r, _ := f.t.Resume(context.Background(), b, "go")
	if r.OK || r.Submitted || r.Code != "headless_start_failed" {
		t.Fatalf("start failure %+v", r)
	}
	if f.t.running[cliSession] {
		t.Fatal("failed start left the session marked running")
	}
}

func TestHeadlessEnvAllowlist(t *testing.T) {
	got := headlessEnv([]string{"HOME=/h", "TMUX=/s,1,0", "TMUX_PANE=%1", "CLAUDE_PID=9", "ANTHROPIC_API_KEY=x", "LANG=zh_CN.UTF-8", "CLAUDE_CONFIG_DIR=/c", "broken"})
	want := []string{"HOME=/h", "LANG=zh_CN.UTF-8", "CLAUDE_CONFIG_DIR=/c", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env %q", got)
	}
	if got := headlessEnv([]string{"PATH=/x"}); !reflect.DeepEqual(got, []string{"PATH=/x"}) {
		t.Fatalf("existing PATH must be kept: %q", got)
	}
}
