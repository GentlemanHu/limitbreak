package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

type sessionRecordingEngine struct {
	recordingEngine
	settings []domain.SessionSettings
	sent     []string
	located  []string
	fail     error
}

func (e *sessionRecordingEngine) UpdateSessionSettings(_ context.Context, id string, s domain.SessionSettings) (domain.Session, error) {
	if e.fail != nil {
		return domain.Session{}, e.fail
	}
	e.settings = append(e.settings, s)
	return domain.Session{ID: id}, nil
}
func (e *sessionRecordingEngine) TestSend(_ context.Context, id, message string) (domain.Job, error) {
	if e.fail != nil {
		return domain.Job{}, e.fail
	}
	e.sent = append(e.sent, message)
	return domain.Job{ID: id + "/test-1", State: domain.Ready}, nil
}
func (e *sessionRecordingEngine) Locate(_ context.Context, id string) (domain.BridgeResult, error) {
	if e.fail != nil {
		return domain.BridgeResult{}, e.fail
	}
	e.located = append(e.located, id)
	return domain.BridgeResult{OK: true, Code: "located"}, nil
}

func setupSessionServer() (*Server, *sessionRecordingEngine) {
	c := config.Default(".")
	c.Token = strings.Repeat("x", 64)
	e := &sessionRecordingEngine{}
	return New(c, e), e
}

func TestSessionRoutes(t *testing.T) {
	s, e := setupSessionServer()
	token := "Bearer " + s.cfg.Token
	host := "127.0.0.1:8318"
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"PATCH", "/api/sessions/s1", `{"host_mode":"hosted","priority":3}`, 200},
		{"PATCH", "/api/sessions/s1", `{"unknown":1}`, 400},
		{"POST", "/api/sessions/s1", `{}`, 405},
		{"POST", "/api/sessions/s1/locate", ``, 200},
		{"POST", "/api/sessions/s1/test-send", ``, 202},
		{"POST", "/api/sessions/s1/test-send", `{"message":"继续"}`, 202},
		{"GET", "/api/sessions/s1/locate", ``, 405},
		{"POST", "/api/sessions/s1/resume", ``, 404},
		{"POST", "/api/sessions/s1/cancel", ``, 200},
		{"PATCH", "/api/sessions/", `{}`, 422},
	}
	for _, c := range cases {
		if w := request(s, c.method, c.path, c.body, token, "", host); w.Code != c.want {
			t.Errorf("%s %s = %d want %d: %s", c.method, c.path, w.Code, c.want, w.Body.String())
		}
	}
	if len(e.settings) != 1 || *e.settings[0].HostMode != "hosted" || *e.settings[0].Priority != 3 {
		t.Fatalf("settings = %+v", e.settings)
	}
	if len(e.sent) != 2 || e.sent[0] != "" || e.sent[1] != "继续" || len(e.located) != 1 || e.calls != 1 {
		t.Fatalf("sent=%q located=%q calls=%d", e.sent, e.located, e.calls)
	}
	if w := request(s, "POST", "/api/sessions/s1/test-send", ``, "", "", host); w.Code != 403 {
		t.Fatalf("unauthenticated test-send = %d", w.Code)
	}
}

func TestSessionErrorsHideInternals(t *testing.T) {
	s, e := setupSessionServer()
	token := "Bearer " + s.cfg.Token
	for _, c := range []struct {
		err  error
		code int
		text string
	}{
		{domain.ErrSessionNotFound, 404, "会话不存在"},
		{domain.UserError("该会话已有测试发送在进行"), 422, "该会话已有测试发送在进行"},
		{errors.New("sqlite: /private/path locked"), 503, "状态未知"},
	} {
		e.fail = c.err
		w := request(s, "POST", "/api/sessions/s1/test-send", ``, token, "", "127.0.0.1:8318")
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.text) || strings.Contains(w.Body.String(), "/private/path") {
			t.Fatalf("%v -> %d %s", c.err, w.Code, w.Body.String())
		}
	}
}
