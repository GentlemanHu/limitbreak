package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func clientBinding(id, session, dir string) domain.Binding {
	return domain.Binding{SessionID: session, Kind: "code", Executor: domain.ClientExecutor(id), WorkDir: dir,
		Origin: &domain.SessionOrigin{App: domain.AppClient, Client: id}}
}

func writeScript(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "client.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// shellCommand runs body through the system shell, so tests that wait for the
// client's output do not depend on how fast macOS admits a freshly written
// executable. Placeholders in args are still substituted as whole arguments.
func shellCommand(body string, args ...string) []string {
	return append([]string{"/bin/sh", "-c", body, "client"}, args...)
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	// Generous: a freshly written script can take seconds to start on a
	// loaded machine (security assessment, -race); success returns at once.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(b), "\n") {
			return string(b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(path)
	t.Fatalf("%s was not written: %q %v", path, b, err)
	return ""
}

func TestCommandSubstitutesWholeArgumentsWithoutShell(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	body := `printf '%s|%s|%s|%s|%s\n' "$1" "$2" "$3" "$LIMITBREAK_SESSION_ID" "$(pwd -P)" > "$4"`
	c := NewCommand(ClientSpec{ID: "codex", Command: shellCommand(body, "{session_id}", "{message}", "{cwd}", out)})
	b := clientBinding("codex", "sess-1", dir)
	if p := c.Preflight(context.Background(), b); !p.OK || !p.AccessibilityGranted {
		t.Fatalf("preflight: %+v", p)
	}
	message := "继续; echo injected $(id)"
	res, err := c.Resume(context.Background(), b, message)
	if err != nil || !res.OK || !res.Submitted || res.Code != "client_submitted" {
		t.Fatalf("resume: %+v %v", res, err)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	want := "sess-1|" + message + "|" + dir + "|sess-1|" + realDir + "\n"
	if got := waitFile(t, out); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestCommandRefusesForeignOrUnsafeTargets(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "exit 0\n")
	c := NewCommand(ClientSpec{ID: "codex", Command: []string{script, "{message}"}})
	for name, b := range map[string]domain.Binding{
		"other client": clientBinding("workbuddy", "s1", dir),
		"bad session":  clientBinding("codex", "../x", dir),
		"no origin":    {SessionID: "s1", Executor: domain.ClientExecutor("codex")},
	} {
		if r := c.Preflight(context.Background(), b); r.OK {
			t.Errorf("%s accepted: %+v", name, r)
		}
	}
	if r, _ := c.Resume(context.Background(), clientBinding("codex", "s1", dir), "-rf"); r.OK || r.Submitted {
		t.Fatalf("option-like message accepted: %+v", r)
	}
	if err := os.Chmod(script, 0o722); err != nil {
		t.Fatal(err)
	}
	if r := c.Preflight(context.Background(), clientBinding("codex", "s1", dir)); r.OK || r.Code != "client_unavailable" {
		t.Fatalf("group-writable executable accepted: %+v", r)
	}
}

func TestCommandIsBusyWhileRunningAndKilledAfterTimeout(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "sleep 30\n")
	c := NewCommand(ClientSpec{ID: "codex", Command: []string{script, "{message}"}, Timeout: 300 * time.Millisecond})
	b := clientBinding("codex", "s1", dir)
	if r, _ := c.Resume(context.Background(), b, "继续"); !r.Submitted {
		t.Fatalf("first run: %+v", r)
	}
	if r, _ := c.Resume(context.Background(), b, "继续"); r.Submitted || r.Code != "session_busy" {
		t.Fatalf("second run while busy: %+v", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		busy := c.running["s1"]
		c.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed-out client command was not killed")
}

func TestRouterRoutesClientsAndNeverUsesDirect(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	direct := &fakeDirect{}
	r := NewRouter(nil, nil).WithDirect(direct).WithClients(map[string]Backend{
		"codex": NewCommand(ClientSpec{ID: "codex", Command: shellCommand(`echo ran > "$1"`, out, "{message}")}),
	})
	b := clientBinding("codex", "s1", dir)
	b.ExecutionDirect = true
	if res, err := r.Resume(context.Background(), b, "继续"); err != nil || !res.Submitted {
		t.Fatalf("client route: %+v %v", res, err)
	}
	waitFile(t, out)
	if direct.calls != 0 {
		t.Fatal("client session went through the direct channel")
	}
	missing := clientBinding("deepseek", "s1", dir)
	if res := r.Preflight(context.Background(), missing); res.OK || res.Code != "executor_unconfigured" {
		t.Fatalf("unconfigured client accepted: %+v", res)
	}
}

type fakeDirect struct{ calls int }

func (f *fakeDirect) Preflight(context.Context, domain.Binding) domain.BridgeResult {
	f.calls++
	return domain.BridgeResult{OK: true, Channel: domain.ChannelDirect}
}

func (f *fakeDirect) Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error) {
	f.calls++
	return domain.BridgeResult{OK: true, Submitted: true, Channel: domain.ChannelDirect}, nil
}
