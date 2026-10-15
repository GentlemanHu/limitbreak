package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/scheduler"
	"quota-session-orchestrator/internal/store"
)

func controlFixture(t *testing.T) (*Server, *store.Store, *scheduler.Engine) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := config.Default(".")
	c.Token = strings.Repeat("x", 64)
	e := scheduler.New(db, nil, nil, c.Policy)
	if err := e.Bind(context.Background(), domain.Binding{SessionID: "private-session", MonitorResources: []domain.ResourceRef{{AccountID: "private-account"}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(context.Background(), func(tx *store.Tx) error {
		s := tx.Sessions["private-session"]
		s.Cwd = "/private/project"
		s.Title = "private-title"
		s.Error = "private-error"
		tx.Sessions[s.ID] = s
		tx.Accounts = []domain.Account{{ID: "private-account", Email: "private-email", AuthIndex: "private-auth", Reason: "private-reason", QuotaError: "private-error", State: "allowed", CheckedAt: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(c, e, WithStore(db)), db, e
}

func issueGrant(t *testing.T, s *Server) string {
	t.Helper()
	w := request(s, "POST", "/api/mcp/grants", `{"scopes":["read","action:cancel"],"session_targets":["private-session"],"ttl_seconds":3600}`, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.AccessToken == "" {
		t.Fatalf("grant issuance HTTP %d", w.Code)
	}
	return out.AccessToken
}

func TestScopedControlRequiresSeparateAppConfirmation(t *testing.T) {
	s, db, _ := controlFixture(t)
	token := issueGrant(t, s)
	st, _ := db.Read(context.Background())
	grant, _ := st.FindControlGrant(token, time.Now().UTC())
	input := localapi.ActionInput{RequestID: "cancel-once", TTLSeconds: 120, Action: store.ControlAction{Operation: "cancel", SessionID: opaque(grant, "s", "private-session"), ExpectedGeneration: st.Sessions["private-session"].Generation}}
	data, _ := json.Marshal(input)
	w := request(s, "POST", "/mcp/requests", string(data), "Bearer "+token, "", "127.0.0.1:8318")
	var out localapi.RequestResponse
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Request.State != store.ControlPending {
		t.Fatalf("request HTTP %d", w.Code)
	}
	before, _ := db.Read(context.Background())
	if before.Sessions["private-session"].State == domain.Cancelled {
		t.Fatal("AI request executed without App approval")
	}
	approve, _ := json.Marshal(map[string]any{"request_id": out.Request.ID, "digest": out.Request.Digest, "expected_generation": input.Action.ExpectedGeneration, "decision": "approve"})
	for _, path := range []string{"/api/mcp/requests/approve", "/api/desktop/execution", "/api/policy/patch"} {
		if w := request(s, "POST", path, string(approve), "Bearer "+token, "", "127.0.0.1:8318"); w.Code != 401 {
			t.Fatalf("scoped token crossed App boundary at %s: %d", path, w.Code)
		}
	}
	for i := 0; i < 3; i++ {
		w = request(s, "POST", "/api/mcp/requests/approve", string(approve), "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
		if w.Code != 200 {
			t.Fatalf("approval HTTP %d: %s", w.Code, w.Body.String())
		}
	}
	after, _ := db.Read(context.Background())
	if after.Sessions["private-session"].Generation != input.Action.ExpectedGeneration+1 || after.ControlRequests[input.RequestID].State != store.ControlCompleted {
		t.Fatal("duplicate approval executed cancellation twice")
	}
}

func TestScopedReadsAreBoundedAndRedacted(t *testing.T) {
	s, _, _ := controlFixture(t)
	token := issueGrant(t, s)
	for _, path := range []string{"/mcp/status", "/mcp/resources", "/mcp/sources", "/mcp/sessions", "/mcp/queue", "/mcp/adapters", "/mcp/diagnostics", "/mcp/requests"} {
		w := request(s, "GET", path, "", "Bearer "+token, "", "127.0.0.1:8318")
		if w.Code != 200 {
			t.Fatalf("read %s: %d", path, w.Code)
		}
		for _, secret := range []string{"private-session", "private-account", "private-email", "private-auth", "private-title", "/private/project", "private-error", "private-reason", token, s.cfg.Token} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("scoped %s leaked %s", path, secret)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("sensitive response cacheable")
		}
	}
	for _, query := range []string{"?limit=101", "?limit=0", "?limit=1&limit=2", "?unknown=1"} {
		if w := request(s, "GET", "/mcp/resources"+query, "", "Bearer "+token, "", "127.0.0.1:8318"); w.Code != 400 {
			t.Fatal("unbounded paging accepted")
		}
	}
}

func TestScopedActionsRejectInventedTargetsAndDuplicateJSON(t *testing.T) {
	s, _, _ := controlFixture(t)
	token := issueGrant(t, s)
	bodies := []string{
		`{"request_id":"r","ttl_seconds":120,"action":{"operation":"cancel","session_id":"private-session","expected_generation":1,"params":{}}}`,
		`{"request_id":"r","ttl_seconds":120,"action":{"operation":"refresh","session_id":"daemon","expected_generation":0,"params":{}}}`,
	}
	for _, body := range bodies {
		w := request(s, "POST", "/mcp/requests", body, "Bearer "+token, "", "127.0.0.1:8318")
		if w.Code != 403 {
			t.Fatalf("ungranted action accepted: %d", w.Code)
		}
	}
	for _, body := range []string{`null`, `{"scopes":[],"scopes":["read"]}`, `{"action":{"params":{"enabled":true,"enabled":false}}}`, `{} {}`} {
		if w := request(s, "POST", "/api/mcp/grants", body, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318"); w.Code != 400 {
			t.Fatalf("ambiguous control JSON accepted: %d", w.Code)
		}
	}
}

type executionRecordingEngine struct {
	recordingEngine
	lease string
}

func (e *executionRecordingEngine) ValidateExecution(_ context.Context, _ string, _ int64, lease, _ string) (bool, error) {
	e.lease = lease
	return true, nil
}
func TestDesktopExecutionAcceptsExactSlashLeaseAndNotScopedAuthority(t *testing.T) {
	c := config.Default(".")
	c.Token = strings.Repeat("x", 64)
	e := &executionRecordingEngine{}
	s := New(c, e)
	body := `{"session_id":"session","generation":2,"lease_id":"engine/session/2","message_hash":"` + strings.Repeat("a", 64) + `"}`
	w := request(s, "POST", "/api/desktop/execution", body, "Bearer "+c.Token, "", "127.0.0.1:8318")
	if w.Code != 200 || e.lease != "engine/session/2" || !strings.Contains(w.Body.String(), `"eligible":true`) {
		t.Fatalf("valid execution lease rejected: %d", w.Code)
	}
	if w := request(s, "POST", "/api/desktop/execution", strings.Replace(body, `"generation":2`, `"generation":0`, 1), "Bearer "+c.Token, "", "127.0.0.1:8318"); w.Code != 422 {
		t.Fatal("zero dispatch generation accepted")
	}
	if validExecutionLease("engine\nsession/2") || validExecutionLease(strings.Repeat("a", 513)) {
		t.Fatal("invalid execution lease accepted")
	}
}

func TestAppPolicyPatchRejectsStaleDigest(t *testing.T) {
	s, _, e := controlFixture(t)
	snap, _ := e.Snapshot(context.Background())
	body := `{"policy_digest":"` + snap.PolicyDigest + `","patch":{"max_running":3}}`
	w := request(s, http.MethodPost, "/api/policy/patch", body, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	if w.Code != 200 {
		t.Fatalf("patch failed: %d %s", w.Code, w.Body.String())
	}
	w = request(s, http.MethodPost, "/api/policy/patch", body, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	if w.Code != 409 {
		t.Fatal("stale policy digest accepted")
	}
}
