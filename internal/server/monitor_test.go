package server

import "testing"

func TestMonitorBindingDoesNotRequireRouteOrDesktopTarget(t *testing.T) {
	s, e := setupServer()
	payload := `{"session_id":"session","monitor_mode":"all","monitor_resources":[{"account_id":"claude","group_id":"weekly"},{"account_id":"codex","group_id":"shared","window_id":"short"}],"auto_resume":false}`
	w := request(s, "POST", "/api/bind", payload, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	if w.Code != 200 || e.calls != 1 {
		t.Fatalf("monitor-only enrollment rejected: %d %s", w.Code, w.Body.String())
	}
	unsafe := `{"session_id":"session","auto_resume":true,"monitor_resources":[{"account_id":"claude"}]}`
	w = request(s, "POST", "/api/bind", unsafe, "Bearer "+s.cfg.Token, "", "127.0.0.1:8318")
	if w.Code != 422 || e.calls != 1 {
		t.Fatal("monitor references authorized unsafe auto-resume")
	}
}
