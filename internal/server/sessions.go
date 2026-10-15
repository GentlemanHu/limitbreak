package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"quota-session-orchestrator/internal/domain"
)

// sessionEngine is the optional per-session control surface of the engine.
type sessionEngine interface {
	UpdateSessionSettings(context.Context, string, domain.SessionSettings) (domain.Session, error)
	TestSend(context.Context, string, string) (domain.Job, error)
	Locate(context.Context, string) (domain.BridgeResult, error)
}

// sessionAction serves PATCH /api/sessions/{id} and
// POST /api/sessions/{id}/{cancel|locate|test-send}.
func (s *Server) sessionAction(w http.ResponseWriter, r *http.Request) {
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if id == "" || len(id) > 256 || strings.ContainsAny(id, "\x00\r\n") {
		fail(w, 422, "invalid_session", "会话标识无效")
		return
	}
	if action == "" {
		if method(w, r, "PATCH") {
			s.sessionSettings(w, r, id)
		}
		return
	}
	switch action {
	case "cancel", "locate", "test-send":
	default:
		fail(w, 404, "not_found", "操作不存在")
		return
	}
	if !method(w, r, "POST") {
		return
	}
	if action == "cancel" {
		if err := s.engine.Cancel(r.Context(), id); err != nil {
			fail(w, 422, "cancel_failed", "取消失败，会话可能不存在")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	engine, ok := s.engine.(sessionEngine)
	if !ok {
		fail(w, 501, "unsupported", "当前后台不支持此会话操作")
		return
	}
	if action == "locate" {
		result, err := engine.Locate(r.Context(), id)
		if err != nil {
			sessionError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": result.OK, "result": result})
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body, true) {
		return
	}
	job, err := engine.TestSend(r.Context(), id, body.Message)
	if err != nil {
		sessionError(w, err)
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true, "job_id": job.ID, "state": job.State})
}

func (s *Server) sessionSettings(w http.ResponseWriter, r *http.Request, id string) {
	engine, ok := s.engine.(sessionEngine)
	if !ok {
		fail(w, 501, "unsupported", "当前后台不支持会话设置")
		return
	}
	var settings domain.SessionSettings
	if !decode(w, r, &settings, true) {
		return
	}
	session, err := engine.UpdateSessionSettings(r.Context(), id, settings)
	if err != nil {
		sessionError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "session": session})
}

// sessionError shows locally authored refusals and hides internal failures.
func sessionError(w http.ResponseWriter, err error) {
	var user domain.UserError
	switch {
	case errors.Is(err, domain.ErrSessionNotFound):
		fail(w, 404, "session_not_found", string(domain.ErrSessionNotFound))
	case errors.As(err, &user):
		fail(w, 422, "session_refused", string(user))
	default:
		fail(w, 503, "session_unavailable", "会话操作失败；状态未知，请刷新后再试")
	}
}
