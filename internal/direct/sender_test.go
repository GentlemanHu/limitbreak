package direct

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/host"
)

const (
	cliID  = "9c4e2a71-5d3b-4f8e-a6c2-1b7d0e9f3a54"
	hostID = "local_3f6a9d20-7b1e-4c5a-9e84-2d0c6b1f8a73"
)

var started = time.Date(2026, 10, 14, 7, 25, 49, 0, time.UTC)

type fixture struct {
	t        *testing.T
	registry string
	socks    string
	sender   *Sender
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes; keep them short.
	root, err := os.MkdirTemp("/tmp", "qd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := &fixture{t: t, registry: filepath.Join(root, "r"), socks: filepath.Join(root, "s")}
	for _, d := range []string{f.registry, f.socks} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.sender = &Sender{
		dirs: func() []string { return []string{f.registry} },
		lookup: func(pid int) (host.Process, bool) {
			if pid == 4242 || pid == 4343 {
				return host.Process{PID: pid, Start: started.Add(300 * time.Millisecond)}, true
			}
			if pid == 5151 {
				return host.Process{PID: pid, Start: started.Add(time.Hour)}, true // PID reused
			}
			return host.Process{}, false
		},
		dial: func(ctx context.Context, path string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
		sleep:   sleepContext,
		uid:     os.Getuid(),
		confirm: 2 * time.Second,
	}
	return f
}

func (f *fixture) write(pid int, e Entry) {
	f.t.Helper()
	e.PID = pid
	if e.ProcStart == "" {
		e.ProcStart = started.Format(procStartLayout)
	}
	if e.PeerProtocol == 0 {
		e.PeerProtocol = 1
	}
	data, err := json.Marshal(e)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.registry, strconv.Itoa(pid)+".json"), data, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) listen(pid int) (string, <-chan string) {
	f.t.Helper()
	path := filepath.Join(f.socks, strconv.Itoa(pid)+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { ln.Close() })
	lines := make(chan string, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			if line, err := r.ReadString('\n'); err == nil {
				lines <- line
			}
			c.Close()
		}
	}()
	return path, lines
}

func desktopBinding() domain.Binding {
	return domain.Binding{SessionID: cliID, Executor: domain.ExecutorDesktop, DeepLink: domain.DesktopDeepLink(hostID),
		Origin: &domain.SessionOrigin{App: domain.AppClaudeDesktop, HostSessionID: hostID}, ExecutionDirect: true}
}

func TestDeliversFrameAndObservesTurn(t *testing.T) {
	f := newFixture(t)
	sock, lines := f.listen(4242)
	entry := Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock, Status: "idle", StatusUpdated: 1}
	f.write(4242, entry)
	if r := f.sender.Preflight(context.Background(), desktopBinding()); !r.OK || r.Code != CodeReady || r.Channel != domain.ChannelDirect {
		t.Fatalf("preflight %+v", r)
	}
	go func() {
		line := <-lines
		var frame map[string]any
		if json.Unmarshal([]byte(line), &frame) != nil {
			t.Errorf("frame is not JSON: %q", line)
		}
		msg, _ := frame["message"].(map[string]any)
		if frame["type"] != "user" || frame["session_id"] != cliID || msg["content"] != "继续" || msg["role"] != "user" || frame["priority"] != "next" {
			t.Errorf("unexpected frame %v", frame)
		}
		if _, hasMode := frame["fromMode"]; hasMode {
			t.Error("client must never assert the receiver's permission mode")
		}
		busy := entry
		busy.Status, busy.StatusUpdated = "busy", 2
		f.write(4242, busy)
	}()
	r, err := f.sender.Resume(context.Background(), desktopBinding(), "继续")
	if err != nil || !r.OK || !r.Submitted || r.Code != CodeDelivered {
		t.Fatalf("resume %+v %v", r, err)
	}
}

func TestUnconfirmedWhenSessionStaysIdle(t *testing.T) {
	f := newFixture(t)
	f.sender.confirm = 300 * time.Millisecond
	sock, lines := f.listen(4242)
	f.write(4242, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock, Status: "idle", StatusUpdated: 1})
	r, err := f.sender.Resume(context.Background(), desktopBinding(), "继续")
	if err != nil || !r.OK || !r.Submitted || r.Code != CodeUnconfirmed {
		t.Fatalf("resume %+v %v", r, err)
	}
	select {
	case <-lines:
	case <-time.After(time.Second):
		t.Fatal("frame never reached the socket")
	}
}

func TestBusySessionIsNotInterrupted(t *testing.T) {
	f := newFixture(t)
	sock, lines := f.listen(4242)
	f.write(4242, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock, Status: "busy"})
	r, _ := f.sender.Resume(context.Background(), desktopBinding(), "继续")
	if r.Submitted || r.Code != CodeBusy || Unavailable(r) {
		t.Fatalf("busy resume %+v", r)
	}
	select {
	case line := <-lines:
		t.Fatalf("busy session received %q", line)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestUnavailableCases(t *testing.T) {
	f := newFixture(t)
	sock, _ := f.listen(5151)
	// No record for the session at all.
	if r := f.sender.Preflight(context.Background(), desktopBinding()); !Unavailable(r) {
		t.Fatalf("missing record %+v", r)
	}
	// A record whose PID now belongs to a later process.
	f.write(5151, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock, Status: "idle"})
	if r := f.sender.Preflight(context.Background(), desktopBinding()); !Unavailable(r) {
		t.Fatalf("reused pid %+v", r)
	}
	// An unknown protocol version.
	os.Remove(filepath.Join(f.registry, "5151.json"))
	sock2, _ := f.listen(4242)
	f.write(4242, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock2, Status: "idle", PeerProtocol: 2})
	if r := f.sender.Preflight(context.Background(), desktopBinding()); !Unavailable(r) {
		t.Fatalf("protocol %+v", r)
	}
	// A socket outside the user's private directory.
	f.write(4242, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: "/tmp/../tmp/x.sock", Status: "idle"})
	if r := f.sender.Preflight(context.Background(), desktopBinding()); !Unavailable(r) {
		t.Fatalf("socket path %+v", r)
	}
	// A dead receiver: nothing is written and the UI channel may take over.
	f.write(4242, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: filepath.Join(f.socks, "gone.sock"), Status: "idle"})
	if r, _ := f.sender.Resume(context.Background(), desktopBinding(), "继续"); !Unavailable(r) {
		t.Fatalf("dead socket %+v", r)
	}
}

func TestIdentityMismatchStops(t *testing.T) {
	f := newFixture(t)
	sock, _ := f.listen(4242)
	f.write(4242, Entry{SessionID: cliID, HostSessionID: "local_00000000-0000-4000-8000-000000000000", SocketPath: sock, Status: "idle"})
	if r := f.sender.Preflight(context.Background(), desktopBinding()); r.Code != CodeAmbiguous || Unavailable(r) {
		t.Fatalf("other host %+v", r)
	}
	terminal := domain.Binding{SessionID: cliID, Executor: domain.ExecutorTerminal, Origin: &domain.SessionOrigin{App: domain.AppTerminal}}
	if r := f.sender.Preflight(context.Background(), terminal); r.Code != CodeAmbiguous {
		t.Fatalf("terminal binding matched a desktop process %+v", r)
	}
	sock2, _ := f.listen(4343)
	f.write(4242, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock, Status: "idle"})
	f.write(4343, Entry{SessionID: cliID, HostSessionID: hostID, SocketPath: sock2, Status: "idle"})
	if r := f.sender.Preflight(context.Background(), desktopBinding()); r.Code != CodeAmbiguous {
		t.Fatalf("two live processes %+v", r)
	}
}

func TestTerminalSessionAndForeignRecords(t *testing.T) {
	f := newFixture(t)
	sock, lines := f.listen(4242)
	f.write(4242, Entry{SessionID: cliID, SocketPath: sock, Status: "idle", StatusUpdated: 1})
	if err := os.WriteFile(filepath.Join(f.registry, "junk.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	terminal := domain.Binding{SessionID: cliID, Executor: domain.ExecutorTerminal, Origin: &domain.SessionOrigin{App: domain.AppTerminal}, ExecutionDirect: true}
	f.sender.confirm = 100 * time.Millisecond
	r, err := f.sender.Resume(context.Background(), terminal, "继续")
	if err != nil || !r.Submitted {
		t.Fatalf("terminal resume %+v %v", r, err)
	}
	<-lines
	// A world-writable registry directory is never trusted.
	if err := os.Chmod(f.registry, 0o777); err != nil {
		t.Fatal(err)
	}
	if r := f.sender.Preflight(context.Background(), terminal); !Unavailable(r) {
		t.Fatalf("writable registry trusted %+v", r)
	}
}

func TestFrameRejectsOversizeAndInvalid(t *testing.T) {
	if _, err := Frame(cliID, string(make([]byte, maxLine)), "u"); err == nil {
		t.Fatal("oversize frame accepted")
	}
	f := newFixture(t)
	if r, _ := f.sender.Resume(context.Background(), desktopBinding(), "a\nb"); r.Code != CodeInvalid || r.Submitted {
		t.Fatalf("multi-line message %+v", r)
	}
}

func TestProcStartIsUTC(t *testing.T) {
	got, ok := Entry{ProcStart: "Wed Oct 14 07:25:49 2026"}.ParseProcStart()
	if !ok || !got.Equal(started) {
		t.Fatalf("%v %v", got, ok)
	}
	if _, ok := (Entry{ProcStart: "1791963618"}).ParseProcStart(); ok {
		t.Fatal("unexpected format accepted")
	}
}
