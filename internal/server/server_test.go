package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

type recordingEngine struct{ calls int }

func (e *recordingEngine) Snapshot(context.Context) (domain.Snapshot, error) {
	return domain.Snapshot{Policy: config.Default(".").Policy}, nil
}
func (e *recordingEngine) Refresh(context.Context) error                       { e.calls++; return nil }
func (e *recordingEngine) HandleEvent(context.Context, domain.HookEvent) error { e.calls++; return nil }
func (e *recordingEngine) Bind(context.Context, domain.Binding) error          { e.calls++; return nil }
func (e *recordingEngine) Cancel(context.Context, string) error                { e.calls++; return nil }
func (e *recordingEngine) SetPolicy(context.Context, domain.Policy) error      { e.calls++; return nil }

func setupServer() (*Server, *recordingEngine) {
	c := config.Default(".")
	c.Token = strings.Repeat("x", 64)
	e := &recordingEngine{}
	return New(c, e), e
}
func request(s *Server, method, path, body, auth, origin, host string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8318"+path, strings.NewReader(body))
	r.Host = host
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestAuthenticationAndOrigin(t *testing.T) {
	s, e := setupServer()
	token := "Bearer " + s.cfg.Token
	cases := []struct {
		method, path, auth, origin, host string
		want                             int
	}{
		{"GET", "/api/snapshot", "", "", "127.0.0.1:8318", 401},
		{"POST", "/api/refresh", token, "https://attacker.example", "127.0.0.1:8318", 403},
		{"POST", "/api/refresh", "", "", "127.0.0.1:8318", 403},
		{"GET", "/api/refresh", token, "", "127.0.0.1:8318", 405},
		{"GET", "/api/health", "", "", "attacker.example:8318", 403},
		{"GET", "/api/health", "", "", "127.0.0.1:9999", 403},
		{"GET", "/action/continue", token, "", "127.0.0.1:8318", 404},
		{"POST", "/api/refresh", token, "", "127.0.0.1:8318", 200},
	}
	for _, c := range cases {
		w := request(s, c.method, c.path, "{}", c.auth, c.origin, c.host)
		if w.Code != c.want {
			t.Errorf("%s %s got %d want %d", c.method, c.path, w.Code, c.want)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("CORS must not be wildcard")
		}
	}
	if e.calls != 1 {
		t.Fatalf("unauthorized operations reached engine: %d", e.calls)
	}
}

func TestHookValidation(t *testing.T) {
	s, e := setupServer()
	token := "Bearer " + s.cfg.Token
	event := domain.HookEvent{EventID: "event", SessionID: "session", HookEventName: "StopFailure", Error: "rate_limit", ObservedAt: time.Now()}
	data, _ := json.Marshal(event)
	w := request(s, "POST", "/api/hooks", string(data), token, "http://127.0.0.1:8318", "127.0.0.1:8318")
	if w.Code != 403 {
		t.Fatal("browser must not inject lifecycle")
	}
	w = request(s, "POST", "/api/hooks", string(data), token, "", "127.0.0.1:8318")
	if w.Code != 200 || e.calls != 1 {
		t.Fatalf("valid event: %d", w.Code)
	}
	event.HookEventName = "MadeUpEvent"
	data, _ = json.Marshal(event)
	w = request(s, "POST", "/api/hooks", string(data), token, "", "127.0.0.1:8318")
	if w.Code != 422 {
		t.Fatal("unknown event accepted")
	}
}

func TestStrictJSONAndBindingSafety(t *testing.T) {
	s, e := setupServer()
	token := "Bearer " + s.cfg.Token
	w := request(s, "POST", "/api/policy", `{"paused":true,"unexpected":true}`, token, "", "127.0.0.1:8318")
	if w.Code != 400 {
		t.Fatal("unknown field accepted")
	}
	w = request(s, "POST", "/api/policy", `{} {}`, token, "", "127.0.0.1:8318")
	if w.Code != 400 {
		t.Fatal("trailing object accepted")
	}
	b := domain.Binding{SessionID: "session", Kind: "chat", Provider: "codex", Model: "configured-alias", AccountIDs: []string{"account"}, AutoResume: true, AcknowledgeSideEffects: true, ProjectMarker: "project", TargetTitle: "title"}
	data, _ := json.Marshal(b)
	w = request(s, "POST", "/api/bind", string(data), token, "", "127.0.0.1:8318")
	if w.Code != 422 || e.calls != 0 {
		t.Fatal("unverified client auto-resume accepted")
	}
}
