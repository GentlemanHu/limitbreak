package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/localapi"
)

type startupPolicyFixture struct {
	policy   domain.Policy
	readErr  error
	writeErr error
	writes   int
}

func (e *startupPolicyFixture) Snapshot(context.Context) (domain.Snapshot, error) {
	return domain.Snapshot{Policy: e.policy}, e.readErr
}
func (e *startupPolicyFixture) SetPolicy(_ context.Context, p domain.Policy) error {
	e.writes++
	e.policy = p
	return e.writeErr
}
func (*startupPolicyFixture) Refresh(context.Context) error { panic("startup must not refresh") }
func (*startupPolicyFixture) HandleEvent(context.Context, domain.HookEvent) error {
	panic("startup must not post hooks")
}
func (*startupPolicyFixture) Bind(context.Context, domain.Binding) error {
	panic("startup must not bind")
}
func (*startupPolicyFixture) Cancel(context.Context, string) error { panic("startup must not cancel") }

func TestStartupPausedPreservesPersistentPolicyAndFailsClosed(t *testing.T) {
	p := config.Default("").Policy
	p.Paused = false
	p.MaxRunning = 3
	expected := p
	expected.Paused = true
	e := &startupPolicyFixture{policy: p}
	if err := pauseBeforeRun(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(e.policy)
	want, _ := json.Marshal(expected)
	if !bytes.Equal(got, want) || e.writes != 1 {
		t.Fatal("startup overwrote persisted settings")
	}
	e = &startupPolicyFixture{policy: p, readErr: errors.New("read failed")}
	if err := pauseBeforeRun(context.Background(), e); err == nil || e.writes != 0 {
		t.Fatal("startup ignored snapshot failure")
	}
	e = &startupPolicyFixture{policy: p, writeErr: errors.New("write failed")}
	if err := pauseBeforeRun(context.Background(), e); err == nil {
		t.Fatal("startup ignored pause failure")
	}
}

func TestMCPArgumentsRejectFullConfigAndNeverEchoSecrets(t *testing.T) {
	for _, args := range [][]string{nil, {"--config", "private-secret"}, {"--token", "qso_scope_private-secret"}, {"--profile", "missing-private-secret"}, {"--profile", "x", "extra-private-secret"}, {"--listen", "127.0.0.1:1"}, {"--profile=private-secret", "--transport=http"}} {
		var stderr bytes.Buffer
		err := mcpCommand(context.Background(), args, &mcp.StdioTransport{}, &stderr)
		if err == nil {
			t.Fatalf("accepted args %v", args)
		}
		if strings.Contains(err.Error(), "private-secret") || stderr.Len() != 0 {
			t.Fatal("CLI leaked arguments")
		}
	}
	// Dispatch before the full-config parser, even with a deliberately missing config.
	if err := execute([]string{"mcp", "--config", "missing-private-secret"}); !errors.Is(err, errMCPArguments) {
		t.Fatalf("wrong branch: %v", err)
	}
}

func TestMCPHelpUsesOnlyStderr(t *testing.T) {
	var stderr bytes.Buffer
	if err := mcpCommand(context.Background(), []string{"--help"}, nil, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "--profile") || !strings.Contains(stderr.String(), "stdio") {
		t.Fatal("missing help")
	}
}

func TestMCPRejectsDaemonConfigAsProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "full-config.json")
	if err := os.WriteFile(path, []byte(`{"token":"full-private-secret","database":"do-not-open.sqlite"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if err := mcpCommand(context.Background(), []string{"--profile", path}, nil, &stderr); !errors.Is(err, localapi.ErrProfile) {
		t.Fatal("full config accepted")
	}
	if stderr.Len() != 0 {
		t.Fatal("profile data leaked")
	}
}

// The child is this test executable, not a shell or production binary. Its only
// profile is the temporary fixture created below; os.Exit avoids testing banners.
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("QSO_MCP_TEST_HELPER") != "1" {
		return
	}
	err := execute([]string{"mcp", "--profile", os.Getenv("QSO_MCP_TEST_PROFILE")})
	if err != nil {
		_, _ = io.WriteString(os.Stderr, "restricted MCP helper failed\n")
		os.Exit(2)
	}
	os.Exit(0)
}

func TestMCPActualStdioProtocolAndDaemonIsolation(t *testing.T) {
	token := "qso_scope_" + strings.Repeat("a", 64)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/status" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("unrestricted request")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(localapi.Status{Time: time.Now().UTC(), Paused: true, PolicyDigest: strings.Repeat("b", 64), ControlRequiresConfirmation: true})
	}))
	defer daemon.Close()
	profile := filepath.Join(t.TempDir(), "restricted.json")
	data, _ := json.Marshal(localapi.Profile{DaemonURL: daemon.URL, AccessToken: token})
	if err := os.WriteFile(profile, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPStdioHelper$")
	cmd.Env = append(os.Environ(), "QSO_MCP_TEST_HELPER=1", "QSO_MCP_TEST_PROFILE="+profile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	write := func(v any) {
		t.Helper()
		if err := json.NewEncoder(stdin).Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id int, allowError ...bool) map[string]json.RawMessage {
		t.Helper()
		if !scanner.Scan() {
			t.Fatalf("missing MCP response: %v", scanner.Err())
		}
		line := scanner.Bytes()
		if bytes.Contains(line, []byte(token)) {
			t.Fatal("token on stdout")
		}
		var reply map[string]json.RawMessage
		if json.Unmarshal(line, &reply) != nil || string(reply["jsonrpc"]) != `"2.0"` {
			t.Fatalf("stdout is not protocol: %s", line)
		}
		var gotID int
		if json.Unmarshal(reply["id"], &gotID) != nil || gotID != id || reply["error"] != nil && (len(allowError) == 0 || !allowError[0]) {
			t.Fatalf("unexpected reply: %s", line)
		}
		return reply
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "isolated-stdio-test", "version": "1"}}})
	read(1)
	write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	list := read(2)
	if !bytes.Contains(list["result"], []byte(`"request_settings_patch"`)) {
		t.Fatal("missing tools")
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "status", "arguments": map[string]any{}}})
	r := read(3)
	if !bytes.Contains(r["result"], []byte(`"control_requires_confirmation":true`)) {
		t.Fatal("missing scoped structured status")
	}
	write(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "request_pause", "arguments": map[string]any{"requestID": "private-prompt", "ttlSeconds": "qso_scope_private-secret"}}})
	errReply := read(4, true)
	errData, _ := json.Marshal(errReply)
	if bytes.Contains(errData, []byte("private-prompt")) || bytes.Contains(errData, []byte("private-secret")) {
		t.Fatal("schema error leaked")
	}
	if !bytes.Contains(errData, []byte("restricted MCP request failed")) {
		t.Fatal("schema error not fixed")
	}
	_ = stdin.Close()
	for scanner.Scan() {
		if !json.Valid(scanner.Bytes()) {
			t.Fatal("non-protocol stdout on shutdown")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("stdio exit: %v; %s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}
