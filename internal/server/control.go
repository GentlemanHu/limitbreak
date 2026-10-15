package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/store"
)

func (s *Server) registerControlRoutes() {
	s.mux.HandleFunc("/api/mcp/grants", s.appControlAuth(s.controlGrants))
	s.mux.HandleFunc("/api/mcp/requests", s.appControlAuth(s.controlRequests))
	s.mux.HandleFunc("/api/mcp/requests/approve", s.appControlAuth(s.controlApprove))
	s.mux.HandleFunc("/api/desktop/execution", s.appControlAuth(s.desktopExecution))
	s.mux.HandleFunc("/api/policy/patch", s.appControlAuth(s.policyPatch))
	s.mux.HandleFunc("/mcp/", s.scoped)
}

func (s *Server) appControlAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") || !s.checkToken(strings.TrimPrefix(h, "Bearer ")) {
			fail(w, 401, "unauthorized", "此操作需要主应用本机令牌")
			return
		}
		next(w, r)
	}
}

func controlError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrControlConflict):
		fail(w, 409, "control_conflict", "参数或目标代次已变化；请重新确认")
	case errors.Is(err, store.ErrControlForbidden):
		fail(w, 403, "control_forbidden", "授权不允许此目标或操作")
	case errors.Is(err, store.ErrControlExpired):
		fail(w, 410, "control_expired", "请求已到期")
	case errors.Is(err, store.ErrControlLimit):
		fail(w, 429, "control_limit", "控制记录已达上限")
	case errors.Is(err, store.ErrControlInvalid):
		fail(w, 422, "control_invalid", "控制字段无效")
	default:
		fail(w, 503, "control_unavailable", "控制结果无法核验；请读取请求记录，勿自动重试")
	}
}

// Strict decoding also rejects duplicate keys: neither UI nor digest may interpret
// the same input differently. The outer object must be non-null.
func decodeControl(w http.ResponseWriter, r *http.Request, out any, max int64) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "content_type", "请求必须为 JSON")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		fail(w, 400, "invalid_json", "请求超限或 JSON 无效")
		return false
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		fail(w, 400, "invalid_json", "需要 JSON 对象")
		return false
	}
	check := json.NewDecoder(bytes.NewReader(data))
	if !uniqueJSON(check, 0) || check.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_json", "字段重复或 JSON 无效")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_json", "JSON 字段无效")
		return false
	}
	return true
}

func uniqueJSON(d *json.Decoder, depth int) bool {
	if depth > 16 {
		return false
	}
	t, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return false
			}
			k, ok := key.(string)
			if !ok || seen[k] {
				return false
			}
			seen[k] = true
			if !uniqueJSON(d, depth+1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for d.More() {
			if !uniqueJSON(d, depth+1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	}
	return false
}

func paging(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	q := r.URL.Query()
	for key, values := range q {
		if key != "cursor" && key != "limit" || len(values) != 1 {
			fail(w, 400, "invalid_page", "分页参数无效")
			return "", 0, false
		}
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			limit = 0
		}
	}
	cursor := q.Get("cursor")
	if limit < 1 || limit > 100 || len(cursor) > 128 {
		fail(w, 400, "invalid_page", "分页参数无效")
		return "", 0, false
	}
	return cursor, limit, true
}

func (s *Server) controlReady(w http.ResponseWriter) bool {
	if s.controlStore == nil {
		fail(w, 503, "control_unavailable", "未配置共享控制存储")
		return false
	}
	return true
}

type grantView struct {
	ID             string     `json:"id"`
	Scopes         []string   `json:"scopes"`
	SessionTargets []string   `json:"session_targets"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

func publicGrant(g store.ControlGrant) grantView {
	return grantView{g.ID, g.Scopes, g.SessionTargets, g.CreatedAt, g.ExpiresAt, g.RevokedAt}
}

func (s *Server) controlGrants(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET", "POST", "DELETE") || !s.controlReady(w) {
		return
	}
	now := time.Now().UTC()
	if r.Method == "GET" {
		cursor, limit, ok := paging(w, r)
		if !ok {
			return
		}
		if err := s.controlStore.Update(r.Context(), func(t *store.Tx) error { t.PruneControl(now); return nil }); err != nil {
			controlError(w, err)
			return
		}
		st, err := s.controlStore.Read(r.Context())
		if err != nil {
			controlError(w, err)
			return
		}
		ids := make([]string, 0, len(st.ControlGrants))
		for id := range st.ControlGrants {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := []grantView{}
		next := ""
		for _, id := range ids {
			if id <= cursor {
				continue
			}
			if len(out) == limit {
				next = out[len(out)-1].ID
				break
			}
			out = append(out, publicGrant(st.ControlGrants[id]))
		}
		writeJSON(w, 200, map[string]any{"grants": out, "next_cursor": next})
		return
	}
	if r.Method == "DELETE" {
		var in struct {
			GrantID string `json:"grant_id"`
		}
		if !decodeControl(w, r, &in, 2048) {
			return
		}
		err := s.controlStore.Update(r.Context(), func(t *store.Tx) error { return t.RevokeControlGrant(in.GrantID, now) })
		if err != nil {
			controlError(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	var in struct {
		Scopes         []string `json:"scopes"`
		SessionTargets []string `json:"session_targets"`
		TTLSeconds     int      `json:"ttl_seconds"`
	}
	if !decodeControl(w, r, &in, 16<<10) {
		return
	}
	if in.TTLSeconds < 60 || in.TTLSeconds > 86400 {
		controlError(w, store.ErrControlInvalid)
		return
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		controlError(w, err)
		return
	}
	secret := "qso_scope_" + hex.EncodeToString(b[:])
	if _, err := rand.Read(b[:]); err != nil {
		controlError(w, err)
		return
	}
	g := store.ControlGrant{ID: "g_" + hex.EncodeToString(b[:16]), TokenHash: store.ControlTokenHash(secret), Scopes: in.Scopes, SessionTargets: in.SessionTargets, CreatedAt: now, ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second)}
	err := s.controlStore.Update(r.Context(), func(t *store.Tx) error { return t.AddControlGrant(g, now) })
	if err != nil {
		controlError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"grant": publicGrant(g), "access_token": secret})
}

func requestView(r store.ControlRequest) localapi.RequestView {
	return localapi.RequestView{ID: r.ID, GrantID: r.GrantID, Action: r.Action, Digest: r.Digest, State: r.State, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, ApprovedAt: r.ApprovedAt, ExecutingAt: r.ExecutingAt, CompletedAt: r.CompletedAt, Result: r.Result}
}

func (s *Server) controlRequests(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") || !s.controlReady(w) {
		return
	}
	s.listControlRequests(w, r, nil)
}

func (s *Server) listControlRequests(w http.ResponseWriter, r *http.Request, g *store.ControlGrant) {
	cursor, limit, ok := paging(w, r)
	if !ok {
		return
	}
	err := s.controlStore.Update(r.Context(), func(t *store.Tx) error { t.PruneControl(time.Now().UTC()); return nil })
	if err != nil {
		controlError(w, err)
		return
	}
	st, err := s.controlStore.Read(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	out := localapi.RequestPage{Requests: []localapi.RequestView{}}
	for _, id := range store.SortedControlRequestIDs(st) {
		v := st.ControlRequests[id]
		if id <= cursor || g != nil && v.GrantID != g.ID {
			continue
		}
		if len(out.Requests) == limit {
			out.NextCursor = out.Requests[len(out.Requests)-1].ID
			break
		}
		view := requestView(v)
		if g != nil {
			view = scopedRequest(view, *g)
		}
		out.Requests = append(out.Requests, view)
	}
	writeJSON(w, 200, out)
}

func (s *Server) controlApprove(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") || !s.controlReady(w) {
		return
	}
	var in struct {
		RequestID          string `json:"request_id"`
		Digest             string `json:"digest"`
		ExpectedGeneration int64  `json:"expected_generation"`
		Decision           string `json:"decision"`
	}
	if !decodeControl(w, r, &in, 2048) {
		return
	}
	if in.Decision != "approve" && in.Decision != "reject" {
		controlError(w, store.ErrControlInvalid)
		return
	}
	var approved store.ControlRequest
	err := s.controlStore.Update(r.Context(), func(t *store.Tx) error {
		var err error
		approved, err = t.ApproveControl(in.RequestID, in.Digest, in.ExpectedGeneration, in.Decision == "approve", time.Now().UTC())
		return err
	})
	if err != nil {
		controlError(w, err)
		return
	}
	if approved.State == store.ControlApproved && in.Decision == "approve" {
		executor, ok := s.engine.(interface {
			ExecuteControl(context.Context, store.ControlRequest) (store.ControlResult, error)
		})
		if !ok {
			fail(w, 503, "control_unavailable", "当前引擎不支持原子控制；未执行操作")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		// No transaction is held here. Engine owns claim/mutation/result atomicity.
		_, execErr := executor.ExecuteControl(ctx, approved)
		if execErr != nil {
			// Once claimed, an error must never permit a second execution.
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			_ = s.controlStore.Update(cleanup, func(t *store.Tx) error {
				v := t.ControlRequests[approved.ID]
				if v.State == store.ControlExecuting {
					return t.FinishControl(v.ID, v.Digest, store.ControlResult{Code: "execution_uncertain"}, time.Now().UTC())
				}
				return nil
			})
			stop()
			controlError(w, execErr)
			return
		}
	}
	st, err := s.controlStore.Read(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	writeJSON(w, 200, localapi.RequestResponse{Request: requestView(st.ControlRequests[approved.ID])})
}

// Execution leases contain engine-owner/session/generation separators, unlike
// control request IDs. This is an identity string, never a filesystem path.
func validExecutionLease(lease string) bool {
	if len(lease) == 0 || len(lease) > 512 {
		return false
	}
	for _, c := range lease {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func (s *Server) desktopExecution(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var in struct {
		SessionID   string `json:"session_id"`
		Generation  int64  `json:"generation"`
		LeaseID     string `json:"lease_id"`
		MessageHash string `json:"message_hash"`
	}
	if !decodeControl(w, r, &in, 2048) {
		return
	}
	if !store.ValidControlID(in.SessionID) || in.Generation <= 0 || !validExecutionLease(in.LeaseID) || len(in.MessageHash) != 64 {
		controlError(w, store.ErrControlInvalid)
		return
	}
	if _, err := hex.DecodeString(in.MessageHash); err != nil {
		controlError(w, store.ErrControlInvalid)
		return
	}
	validator, ok := s.engine.(interface {
		ValidateExecution(context.Context, string, int64, string, string) (bool, error)
	})
	if !ok {
		fail(w, 503, "execution_unavailable", "当前引擎无法验证执行资格")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	eligible, err := validator.ValidateExecution(ctx, in.SessionID, in.Generation, in.LeaseID, in.MessageHash)
	if err != nil {
		fail(w, 503, "execution_unavailable", "执行资格无法核验；未执行发送")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true, "eligible": eligible})
}
