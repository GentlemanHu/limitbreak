package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/scheduler"
	"quota-session-orchestrator/internal/server"
	"quota-session-orchestrator/internal/store"
)

// Real HTTP, shared SQLite and the official SDK are exercised together. This
// fixture has no production configuration, credential, daemon or AX executor.
func TestSDKRestrictedHTTPAndAppApprovalIntegration(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := config.Default(".")
	c.Token = strings.Repeat("x", 64)
	e := scheduler.New(db, nil, nil, c.Policy)
	if err := e.Bind(ctx, domain.Binding{SessionID: "native-session", MonitorResources: []domain.ResourceRef{{AccountID: "native-account"}}}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(nil)
	c.Listen = ts.Listener.Addr().String()
	ts.Config.Handler = server.New(c, e, server.WithStore(db))
	ts.Start()
	t.Cleanup(ts.Close)
	post := func(path, token string, data []byte) []byte {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, "POST", ts.URL+path, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		res, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 256<<10))
		if err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
			t.Fatalf("isolated API %s HTTP %d", path, res.StatusCode)
		}
		return body
	}
	grantData := post("/api/mcp/grants", c.Token, []byte(`{"scopes":["read","action:cancel"],"session_targets":["native-session"],"ttl_seconds":3600}`))
	var grant struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(grantData, &grant) != nil {
		t.Fatal("grant response")
	}
	backend, err := localapi.New(localapi.Profile{DaemonURL: ts.URL, AccessToken: grant.AccessToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backend.Close)
	page, err := backend.List(ctx, "sessions", "", 50)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("scoped sessions: %+v %v", page, err)
	}
	if page.Items[0].ID == "native-session" {
		t.Fatal("native identity leaked")
	}
	mcpCtx, cs := connect(t, backend)
	result := call(t, mcpCtx, cs, "request_cancel", SessionInput{RequestID: "cancel-via-sdk", TTLSeconds: 120, SessionID: page.Items[0].ID, ExpectedGeneration: page.Items[0].Generation})
	if result.IsError || !strings.Contains(encoded(t, result), `"state":"pending"`) {
		t.Fatalf("SDK request did not create pending confirmation: %s", encoded(t, result))
	}
	st, _ := db.Read(ctx)
	if st.Sessions["native-session"].State == domain.Cancelled {
		t.Fatal("SDK self-approved operation")
	}
	r := st.ControlRequests["cancel-via-sdk"]
	approval, _ := json.Marshal(map[string]any{"request_id": r.ID, "digest": r.Digest, "expected_generation": r.Action.ExpectedGeneration, "decision": "approve"})
	post("/api/mcp/requests/approve", c.Token, approval)
	result = call(t, mcpCtx, cs, "request_cancel", SessionInput{RequestID: "cancel-via-sdk", TTLSeconds: 120, SessionID: page.Items[0].ID, ExpectedGeneration: page.Items[0].Generation})
	if result.IsError || !strings.Contains(encoded(t, result), `"state":"completed"`) {
		t.Fatal("SDK could not retrieve idempotent completion")
	}
	after, _ := db.Read(ctx)
	if after.Sessions["native-session"].Generation != page.Items[0].Generation+1 {
		t.Fatal("SDK replay repeated cancellation")
	}
	for _, value := range []string{encoded(t, result), encoded(t, call(t, mcpCtx, cs, "requests", PageInput{Limit: 50}))} {
		if strings.Contains(value, "native-session") || strings.Contains(value, grant.AccessToken) || strings.Contains(value, c.Token) {
			t.Fatal("protocol leaked private identity or credential")
		}
	}
}
