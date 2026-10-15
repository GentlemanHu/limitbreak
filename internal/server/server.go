package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"quota-session-orchestrator/internal/adapters"
	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

type Engine interface {
	Snapshot(context.Context) (domain.Snapshot, error)
	Refresh(context.Context) error
	HandleEvent(context.Context, domain.HookEvent) error
	Bind(context.Context, domain.Binding) error
	Cancel(context.Context, string) error
	SetPolicy(context.Context, domain.Policy) error
}

type Server struct {
	cfg          config.Config
	engine       Engine
	mux          *http.ServeMux
	mu           sync.Mutex
	refreshing   bool
	controlStore *store.Store
	signalSecret []byte
}

type Option func(*Server)

func WithStore(db *store.Store) Option { return func(s *Server) { s.controlStore = db } }

func New(c config.Config, e Engine, options ...Option) *Server {
	s := &Server{cfg: c, engine: e, mux: http.NewServeMux()}
	for _, option := range options {
		option(s)
	}
	s.registerControlRoutes()
	s.mux.HandleFunc("/api/health", s.health)
	s.mux.HandleFunc("/api/snapshot", s.auth(s.snapshot))
	s.mux.HandleFunc("/api/adapters", s.auth(s.adapterList))
	s.mux.HandleFunc("/api/sources", s.auth(s.sourceList))
	s.mux.HandleFunc("/api/events", s.auth(s.events))
	s.mux.HandleFunc("/api/hooks", s.auth(s.hooks))
	s.mux.HandleFunc("/api/refresh", s.auth(s.refresh))
	s.mux.HandleFunc("/api/signals", s.signals)
	s.mux.HandleFunc("/api/desktop/probe", s.auth(s.desktopProbe))
	s.mux.HandleFunc("/api/bind", s.auth(s.bind))
	s.mux.HandleFunc("/api/policy", s.auth(s.policy))
	s.mux.HandleFunc("/api/sessions/", s.auth(s.sessionAction))
	s.mux.HandleFunc("/", s.notFound)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if !s.validHost(r.Host) {
		fail(w, 403, "invalid_host", "仅允许本机访问")
		return
	}
	// There is no browser UI: any browser-originated request is refused.
	if r.Header.Get("Origin") != "" {
		fail(w, 403, "invalid_origin", "拒绝浏览器跨源请求")
		return
	}
	// A signed reset-signal webhook is the only write accepted without the
	// local token; its handler verifies the HMAC before reading the payload.
	signed := r.URL.Path == "/api/signals" && r.Header.Get(SignatureHeader) != ""
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !signed && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		fail(w, 403, "missing_token", "写操作必须携带本地令牌")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) validHost(hostport string) bool {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	_, expected, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil || p != expected {
		return false
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !s.checkToken(token) {
			fail(w, 401, "unauthorized", "需要本地令牌")
			return
		}
		next(w, r)
	}
}

func (s *Server) checkToken(token string) bool {
	return len(s.cfg.Token) >= 32 && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) == 1
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "service": "quota-session-orchestrator"})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	d, err := s.engine.Snapshot(r.Context())
	if err != nil {
		fail(w, 500, "snapshot_failed", "无法读取状态")
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "stream_unavailable", "当前连接不支持实时更新")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		d, err := s.engine.Snapshot(r.Context())
		if err == nil {
			b, err := json.Marshal(d)
			if err != nil {
				return
			}
			if _, err = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
				return
			}
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) hooks(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	// Browser sessions may observe state but must not impersonate lifecycle events.
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("Origin") != "" {
		fail(w, 403, "hook_identity", "事件入口仅允许本地 Hook 客户端")
		return
	}
	var e domain.HookEvent
	if !decode(w, r, &e, false) {
		return
	}
	if len(e.SessionID) == 0 || len(e.SessionID) > 256 || strings.ContainsAny(e.SessionID, "\x00\r\n/") {
		fail(w, 422, "invalid_session", "会话标识无效")
		return
	}
	if e.EventID == "" || len(e.EventID) > 256 {
		fail(w, 422, "invalid_event", "必须提供幂等事件标识")
		return
	}
	if e.ObservedAt.IsZero() || e.ObservedAt.After(time.Now().Add(time.Minute)) {
		fail(w, 422, "invalid_time", "事件观测时间无效")
		return
	}
	switch e.HookEventName {
	case "Notification":
		switch e.NotificationType {
		case "quota_auto_resume_fired", "quota_auto_resume_stale", "quota_auto_resume_disabled":
		default:
			fail(w, 422, "unsupported_notification", "不支持此配额通知")
			return
		}
	case "SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "PostModelSwitch":
	default:
		fail(w, 422, "unsupported_event", "不支持此生命周期事件")
		return
	}
	// This endpoint enrolls Code lifecycle hooks; legacy clients predate the kind field.
	if e.Kind == "" {
		e.Kind = "code"
	}
	observer, err := adapters.NewRegistry().Observer("codehooks", s.engine.HandleEvent)
	if err != nil {
		fail(w, 503, "observer_unavailable", "未配置 Code Hook 观察接口")
		return
	}
	if err := observer.Observe(r.Context(), e); err != nil {
		fail(w, 422, "event_rejected", "事件无法应用，请检查会话及事件顺序")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		fail(w, 409, "refresh_busy", "额度查询已在运行")
		return
	}
	s.refreshing = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.refreshing = false; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	refresh := s.engine.Refresh
	if force, ok := s.engine.(interface{ RefreshNow(context.Context) error }); ok {
		refresh = force.RefreshNow
	}
	if err := refresh(ctx); err != nil {
		fail(w, 502, "proxy_unavailable", "代理查询失败，查看诊断与事件记录")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) desktopProbe(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	prober, ok := s.engine.(interface{ ProbeDesktop(context.Context) error })
	if !ok {
		fail(w, 503, "bridge_unavailable", "未配置桌面检查器")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := prober.ProbeDesktop(ctx); err != nil {
		fail(w, 503, "probe_failed", "无法读取桌面环境，未执行任何发送")
		return
	}
	snapshot, err := s.engine.Snapshot(ctx)
	if err != nil {
		fail(w, 503, "probe_result_unavailable", "检查已结束，但结果无法核验；未执行发送")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": "completed", "result": snapshot.Capabilities})
}

func (s *Server) bind(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var b domain.Binding
	if !decode(w, r, &b, true) {
		return
	}
	if b.SessionID == "" || len(b.SessionID) > 256 || b.Kind != "" && b.Kind != "code" && b.Kind != "chat" && b.Kind != "cowork" {
		fail(w, 422, "invalid_binding", "会话与类型无效")
		return
	}
	if b.Priority < -100 || b.Priority > 100 || len(b.AccountIDs) > 256 || len(b.MonitorResources) > 256 || len(b.ResumeResources) > 256 || b.AutoResume && (b.Provider == "" || b.Model == "" || len(b.AccountIDs) == 0) {
		fail(w, 422, "invalid_route", "监控可跨来源组合；自动恢复必须另外指定供应商、模型与账号池")
		return
	}
	if b.AutoResume && (b.Kind != "code" || !b.AcknowledgeSideEffects || (b.TargetTitle == "" && b.TargetIdentifier == "" && b.DeepLink == "")) {
		fail(w, 422, "unsafe_binding", "自动恢复仅允许已明确确认副作用和桌面目标的 Code 会话")
		return
	}
	if len(b.TargetTitle) > 500 || len(b.TargetIdentifier) > 500 || len(b.ProjectMarker) > 500 {
		fail(w, 422, "invalid_target", "桌面目标标识过长")
		return
	}
	if b.DeepLink != "" {
		u, err := url.Parse(b.DeepLink)
		if err != nil || u.Scheme != "claude" || len(b.DeepLink) > 2048 {
			fail(w, 422, "invalid_link", "只能绑定用户提供的 Claude 会话链接")
			return
		}
	}
	if err := domain.ValidateResumeFields(b); err != nil {
		fail(w, 422, "unsafe_binding", "恢复字段未通过目标与所有权核验")
		return
	}
	if err := s.engine.Bind(r.Context(), b); err != nil {
		fail(w, 422, "binding_rejected", "绑定失败，请检查会话或目标")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) policy(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var p domain.Policy
	if !decode(w, r, &p, true) {
		return
	}
	if err := config.ValidatePolicy(p); err != nil {
		fail(w, 422, "invalid_policy", err.Error())
		return
	}
	if err := s.engine.SetPolicy(r.Context(), p); err != nil {
		fail(w, 500, "policy_failed", "策略操作未完全确认，请读取最新快照后再重试")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	fail(w, 404, "not_found", "接口不存在")
}

func decode(w http.ResponseWriter, r *http.Request, out any, strict bool) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		fail(w, 415, "content_type", "请求必须为 JSON")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(out); err != nil {
		fail(w, 400, "invalid_json", "JSON 格式或字段无效")
		return false
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		fail(w, 400, "invalid_json", "只允许单个 JSON 对象")
		return false
	}
	return true
}

func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	fail(w, 405, "method_not_allowed", "请求方法不允许")
	return false
}

func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "code": code, "message": message})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
