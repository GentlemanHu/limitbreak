package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/store"
)

const policyDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

var sessionHandle = "s_" + strings.Repeat("b", 48)
var accountHandle = "a_" + strings.Repeat("c", 48)

// This backend has no connection, Store, Engine, credentials or execution API.
type isolatedBackend struct {
	mu         sync.Mutex
	actions    []localapi.ActionInput
	pages      []string
	requests   map[string]localapi.RequestView
	err        error
	unsafePage bool
}

func (b *isolatedBackend) Status(context.Context) (localapi.Status, error) {
	return localapi.Status{Time: time.Now().UTC(), Paused: true, PolicyDigest: policyDigest, ControlRequiresConfirmation: true}, b.err
}
func (b *isolatedBackend) List(_ context.Context, collection, _ string, _ int) (localapi.Page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pages = append(b.pages, collection)
	if b.unsafePage {
		return localapi.Page{Items: []localapi.Item{{ID: "secret@example.invalid", State: "private-prompt"}}}, nil
	}
	return localapi.Page{Items: []localapi.Item{}}, b.err
}
func (b *isolatedBackend) Requests(context.Context, string, int) (localapi.RequestPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := localapi.RequestPage{Requests: []localapi.RequestView{}}
	for _, r := range b.requests {
		out.Requests = append(out.Requests, r)
	}
	return out, b.err
}
func (b *isolatedBackend) RequestAction(_ context.Context, in localapi.ActionInput) (localapi.RequestResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return localapi.RequestResponse{}, b.err
	}
	if b.requests == nil {
		b.requests = map[string]localapi.RequestView{}
	}
	if old, ok := b.requests[in.RequestID]; ok {
		if store.ControlDigest(in.Action) != old.Digest {
			return localapi.RequestResponse{}, errors.New("private-body conflict")
		}
		return localapi.RequestResponse{Request: old}, nil
	}
	b.actions = append(b.actions, in)
	now := time.Now().UTC()
	r := localapi.RequestView{ID: in.RequestID, GrantID: "never-expose-grant", Action: in.Action, Digest: store.ControlDigest(in.Action), State: store.ControlPending, CreatedAt: now, ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second)}
	b.requests[in.RequestID] = r
	return localapi.RequestResponse{Request: r}, nil
}

func connect(t *testing.T, b Backend) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := New(b).Connect(ctx, st, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "isolated-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		cancel()
		_ = ss.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close(); cancel() })
	return ctx, cs
}
func call(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func encoded(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func rejected(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args any) {
	t.Helper()
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err == nil && !r.IsError {
		t.Fatalf("%s was accepted", name)
	}
	text := encoded(t, r)
	if err != nil {
		text += err.Error()
	}
	for _, secret := range []string{"private-prompt", "private-body", "secret@example.invalid", "qso_scope_", "/private/project", "never-expose-grant"} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked %q", secret)
		}
	}
	if !strings.Contains(text, ErrRequest.Error()) {
		t.Fatalf("error not fixed: %s", text)
	}
}

func TestProtocolInventoryAndReadTools(t *testing.T) {
	b := &isolatedBackend{}
	ctx, cs := connect(t, b)
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"status": true, "resources": true, "sources": true, "sessions": true, "queue": true, "adapters": true, "diagnostics": true, "requests": true, "request_refresh": false, "request_pause": false, "request_unpause": false, "request_cancel": false, "request_bind_monitor": false, "request_enable_auto_resume": false, "request_settings_patch": false, "request_session_settings": false, "request_test_send": false, "request_locate": false}
	if len(list.Tools) != len(want) {
		t.Fatalf("tools = %d", len(list.Tools))
	}
	for _, tool := range list.Tools {
		read, ok := want[tool.Name]
		if !ok || tool.Annotations == nil || tool.Annotations.ReadOnlyHint != read || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Fatalf("bad tool: %s", tool.Name)
		}
		if read {
			r := call(t, ctx, cs, tool.Name, map[string]any{})
			if r.IsError || r.StructuredContent == nil {
				t.Fatalf("read failed: %s", tool.Name)
			}
		}
	}
	if len(b.actions) != 0 || len(b.pages) != 6 {
		t.Fatal("read tools performed writes")
	}
	for _, name := range []string{"approve", "resume", "refresh", "http", "shell", "hook", "settingsPatch"} {
		rejected(t, ctx, cs, name, map[string]any{})
	}
}

func TestControlsArePendingTypedCASAndIdempotent(t *testing.T) {
	b := &isolatedBackend{}
	ctx, cs := connect(t, b)
	cases := []struct {
		name      string
		args      any
		operation string
	}{
		{"request_refresh", RefreshInput{"req-refresh", 300}, "refresh"},
		{"request_pause", PolicyInput{"req-pause", 60, policyDigest}, "pause"},
		{"request_unpause", PolicyInput{"req-unpause", 60, policyDigest}, "unpause"},
		{"request_cancel", SessionInput{"req-cancel", 60, sessionHandle, 7}, "cancel"},
		{"request_bind_monitor", MonitorInput{"req-monitor", 60, sessionHandle, 7, "all", []domain.ResourceRef{{AccountID: accountHandle}}}, "bind_monitor"},
		{"request_enable_auto_resume", AutoResumeInput{"req-auto", 60, sessionHandle, 7, true}, "enable_auto_resume"},
		{"request_settings_patch", SettingsInput{"req-settings", 60, policyDigest, map[string]int{"max_running": 2}}, "settingsPatch"},
		{"request_session_settings", SessionSettingsInput{RequestID: "req-session", TTLSeconds: 60, SessionID: sessionHandle, ExpectedGeneration: 7, HostMode: ptr("hosted"), Priority: ptr(5)}, "session_settings"},
		{"request_test_send", TestSendInput{RequestID: "req-test", TTLSeconds: 60, SessionID: sessionHandle, ExpectedGeneration: 7, Message: "继续"}, "test_send"},
		{"request_locate", SessionInput{"req-locate", 60, sessionHandle, 7}, "locate"},
	}
	for _, tc := range cases {
		r := call(t, ctx, cs, tc.name, tc.args)
		if r.IsError {
			t.Fatalf("control failed: %s", tc.name)
		}
		var out RequestOutput
		if err := json.Unmarshal([]byte(encoded(t, r.StructuredContent)), &out); err != nil {
			t.Fatal(err)
		}
		if out.Request.State != store.ControlPending || !out.RequiresConfirmation || out.Request.Operation != tc.operation {
			t.Fatalf("not pending: %+v", out)
		}
		if strings.Contains(encoded(t, r), "never-expose-grant") {
			t.Fatal("grant leaked")
		}
		again := call(t, ctx, cs, tc.name, tc.args)
		if again.IsError || encoded(t, again.StructuredContent) != encoded(t, r.StructuredContent) {
			t.Fatal("idempotency changed output")
		}
	}
	if len(b.actions) != 10 {
		t.Fatalf("created %d requests", len(b.actions))
	}
	if b.actions[4].Action.SessionID != sessionHandle || b.actions[4].Action.Params.MonitorResources[0].AccountID != accountHandle {
		t.Fatal("scoped handle remapped")
	}
	rejected(t, ctx, cs, "request_settings_patch", SettingsInput{"req-settings", 60, policyDigest, map[string]int{"max_running": 3}})
	r := call(t, ctx, cs, "requests", PageInput{})
	if r.IsError || strings.Contains(encoded(t, r), "never-expose-grant") {
		t.Fatal("unsafe request listing")
	}
}

func TestRejectUnsafeInputsAndRedactAllErrorPaths(t *testing.T) {
	b := &isolatedBackend{}
	ctx, cs := connect(t, b)
	for _, args := range []any{RefreshInput{"req", 0}, RefreshInput{"req", 301}, map[string]any{"requestID": "private-prompt", "ttlSeconds": "qso_scope_secret"}} {
		rejected(t, ctx, cs, "request_refresh", args)
	}
	for _, args := range []any{PolicyInput{"req", 60, ""}, PolicyInput{"req", 60, "private-prompt"}} {
		rejected(t, ctx, cs, "request_pause", args)
	}
	rejected(t, ctx, cs, "request_cancel", SessionInput{"req", 60, "native-private-session", 1})
	rejected(t, ctx, cs, "request_cancel", SessionInput{"req", 60, sessionHandle, -1})
	rejected(t, ctx, cs, "request_settings_patch", SettingsInput{"req", 60, policyDigest, map[string]int{"continue_message": 1}})
	rejected(t, ctx, cs, "request_settings_patch", SettingsInput{"req", 60, policyDigest, map[string]int{"max_running": 86401}})
	rejected(t, ctx, cs, "request_test_send", TestSendInput{RequestID: "req", TTLSeconds: 60, SessionID: sessionHandle, ExpectedGeneration: 7, Message: "继续\n/clear"})
	rejected(t, ctx, cs, "request_session_settings", SessionSettingsInput{RequestID: "req", TTLSeconds: 60, SessionID: sessionHandle, ExpectedGeneration: 7})
	rejected(t, ctx, cs, "request_session_settings", SessionSettingsInput{RequestID: "req", TTLSeconds: 60, SessionID: sessionHandle, ExpectedGeneration: 7, TargetOverride: ptr("http://x")})
	rejected(t, ctx, cs, "resources", PageInput{Limit: 101})
	rejected(t, ctx, cs, "resources", PageInput{Cursor: strings.Repeat("x", 129)})
	rejected(t, ctx, cs, "status", map[string]any{"private-prompt": strings.Repeat("x", 17<<10)})
	if len(b.actions) != 0 || len(b.pages) != 0 {
		t.Fatal("invalid request reached backend")
	}
	b.err = errors.New("qso_scope_secret private-prompt private-body secret@example.invalid /private/project")
	rejected(t, ctx, cs, "status", EmptyInput{})
	b.err = nil
	b.unsafePage = true
	rejected(t, ctx, cs, "sessions", PageInput{})
}

func ptr[T any](v T) *T { return &v }
