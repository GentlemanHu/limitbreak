package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/store"
)

// Handles are per-grant and never encode private account identifiers or paths.
func opaque(g store.ControlGrant, kind, id string) string {
	if id == "" {
		return ""
	}
	h := hmac.New(sha256.New, []byte(g.TokenHash))
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(id))
	return kind + "_" + hex.EncodeToString(h.Sum(nil)[:24])
}

func allowedSession(g store.ControlGrant, id string) bool {
	for _, target := range g.SessionTargets {
		if target == id {
			return true
		}
	}
	return false
}

func safeState(state string) string {
	switch state {
	case domain.Running, domain.Completed, domain.Cooldown, domain.Ready, domain.Dispatching, domain.Verifying, domain.Attention, domain.Cancelled,
		"healthy", "unknown", "degraded", "unavailable", "allowed", "limited", "stale", "disabled", "missing", "error", "active", "connected", "disconnected":
		return state
	}
	return "unknown"
}

func safeKind(kind string) string {
	switch kind {
	case "code", "chat", "cowork", "cliproxyapi", "readonly-http", "antigravity", "quotio", "executor", "observer", "quota", "quota_source", "session_observer", "resume_executor", "reset_signal":
		return kind
	}
	return "unknown"
}

func (s *Server) scoped(w http.ResponseWriter, r *http.Request) {
	if !s.controlReady(w) {
		return
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		fail(w, 401, "unauthorized", "需要独立访问授权")
		return
	}
	st, err := s.controlStore.Read(r.Context())
	if err != nil {
		controlError(w, err)
		return
	}
	g, ok := st.FindControlGrant(strings.TrimPrefix(h, "Bearer "), time.Now().UTC())
	if !ok {
		fail(w, 401, "unauthorized", "访问授权无效或已到期")
		return
	}
	switch r.URL.Path {
	case "/mcp/requests":
		if !method(w, r, "GET", "POST") {
			return
		}
		if r.Method == "GET" {
			s.listControlRequests(w, r, &g)
		} else {
			s.scopedAction(w, r, g)
		}
		return
	case "/mcp/status", "/mcp/resources", "/mcp/sources", "/mcp/sessions", "/mcp/queue", "/mcp/adapters", "/mcp/diagnostics":
	default:
		fail(w, 404, "not_found", "受限接口不存在")
		return
	}
	if !method(w, r, "GET") {
		return
	}
	cursor, limit, ok := paging(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snap, err := s.engine.Snapshot(ctx)
	if err != nil {
		fail(w, 503, "observation_unavailable", "当前状态无法读取")
		return
	}
	if r.URL.Path == "/mcp/status" {
		sessions, jobs := 0, 0
		for _, v := range snap.Sessions {
			if allowedSession(g, v.ID) {
				sessions++
			}
		}
		for _, v := range snap.Jobs {
			if allowedSession(g, v.SessionID) {
				jobs++
			}
		}
		c := snap.Capabilities
		writeJSON(w, 200, localapi.Status{Time: snap.Time, Paused: snap.Policy.Paused, PolicyDigest: store.PolicyDigest(snap.Policy), Sessions: sessions, Jobs: jobs, Accounts: len(snap.Accounts), ProxyConnected: c.ProxyConnected, BridgeChecked: c.BridgeChecked, BridgeAvailable: c.BridgeAvailable, AccessibilityGranted: c.AccessibilityGranted, ScreenLocked: c.ScreenLocked, DesktopCheckedAt: c.DesktopCheckedAt, ControlRequiresConfirmation: true})
		return
	}
	items := safeItems(strings.TrimPrefix(r.URL.Path, "/mcp/"), snap, g)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	out := localapi.Page{Items: []localapi.Item{}}
	for _, item := range items {
		if item.ID <= cursor {
			continue
		}
		if len(out.Items) == limit {
			out.NextCursor = out.Items[len(out.Items)-1].ID
			break
		}
		out.Items = append(out.Items, item)
	}
	writeJSON(w, 200, out)
}

func safeItems(collection string, snap domain.Snapshot, g store.ControlGrant) []localapi.Item {
	out := []localapi.Item{}
	switch collection {
	case "sources":
		sources := snap.Sources
		if len(sources) == 0 {
			sources = snap.Capabilities.Sources
		}
		for _, v := range sources {
			checked := v.CheckedAt
			out = append(out, localapi.Item{ID: opaque(g, "src", v.ID), Kind: safeKind(v.Type), State: safeState(v.State), Connected: v.Connected, HasError: v.Error != "", CheckedAt: &checked})
		}
	case "sessions":
		for _, v := range snap.Sessions {
			if !allowedSession(g, v.ID) {
				continue
			}
			updated := v.UpdatedAt
			item := localapi.Item{ID: opaque(g, "s", v.ID), Kind: safeKind(v.Kind), State: safeState(v.State), Generation: v.Generation, HasError: v.Error != "", CheckedAt: &updated}
			if v.Binding != nil {
				item.AutoResume = v.Binding.AutoResume
			}
			out = append(out, item)
		}
	case "queue":
		for _, v := range snap.Jobs {
			if !allowedSession(g, v.SessionID) {
				continue
			}
			at := v.NotBefore
			out = append(out, localapi.Item{ID: opaque(g, "j", v.ID), SessionID: opaque(g, "s", v.SessionID), State: safeState(v.State), Generation: v.Generation, NotBefore: &at})
		}
	case "adapters":
		for _, v := range snap.Adapters {
			out = append(out, localapi.Item{ID: opaque(g, "adapter", v.ID), Kind: safeKind(v.Kind), Implemented: v.Implemented, ReadOnly: v.ReadOnly})
		}
	case "diagnostics":
		for _, v := range snap.Sessions {
			if allowedSession(g, v.ID) && (v.Error != "" || v.State == domain.Attention) {
				out = append(out, localapi.Item{ID: opaque(g, "diag", v.ID), SessionID: opaque(g, "s", v.ID), State: safeState(v.State), HasError: v.Error != ""})
			}
		}
	case "resources":
		for _, a := range snap.Accounts {
			checked := a.CheckedAt
			account := opaque(g, "a", a.ID)
			out = append(out, localapi.Item{ID: account, AccountID: account, SourceID: opaque(g, "src", a.SourceID), State: safeState(a.State), HasError: a.QuotaError != "", CheckedAt: &checked, AvailableAt: a.AvailableAt})
			for _, group := range a.Groups {
				checked := group.CheckedAt
				groupID := opaque(g, "grp", a.ID+"\x00"+group.ID)
				out = append(out, localapi.Item{ID: groupID, AccountID: account, GroupID: groupID, State: safeState(group.State), CheckedAt: &checked, AvailableAt: group.AvailableAt})
				for _, window := range group.Windows {
					out = append(out, windowItem(g, a.ID, group.ID, window))
				}
			}
			for _, window := range a.Windows {
				out = append(out, windowItem(g, a.ID, "", window))
			}
		}
	}
	return out
}

func windowItem(g store.ControlGrant, account, group string, w domain.Window) localapi.Item {
	id := opaque(g, "win", account+"\x00"+group+"\x00"+w.ID)
	item := localapi.Item{ID: id, AccountID: opaque(g, "a", account), WindowID: id, Blocked: w.Blocked, AvailableAt: w.ResetAt, State: "unknown"}
	if group != "" {
		item.GroupID = opaque(g, "grp", account+"\x00"+group)
	}
	if w.UsedPercent != nil && *w.UsedPercent >= 0 && *w.UsedPercent <= 100 {
		item.UsedPercent = w.UsedPercent
		item.State = "allowed"
		if w.Blocked || *w.UsedPercent >= 100 {
			item.State = "limited"
		}
	}
	return item
}

func scopedRequest(v localapi.RequestView, g store.ControlGrant) localapi.RequestView {
	if v.Action.SessionID != "daemon" {
		v.Action.SessionID = opaque(g, "s", v.Action.SessionID)
	}
	refs := make([]domain.ResourceRef, 0, len(v.Action.Params.MonitorResources))
	for _, ref := range v.Action.Params.MonitorResources {
		safe := domain.ResourceRef{AccountID: opaque(g, "a", ref.AccountID)}
		if ref.GroupID != "" {
			safe.GroupID = opaque(g, "grp", ref.AccountID+"\x00"+ref.GroupID)
		}
		if ref.WindowID != "" {
			safe.WindowID = opaque(g, "win", ref.AccountID+"\x00"+ref.GroupID+"\x00"+ref.WindowID)
		}
		refs = append(refs, safe)
	}
	v.Action.Params.MonitorResources = refs
	return v
}

func resolveAction(st *store.State, g store.ControlGrant, a localapi.ActionInput) (store.ControlAction, error) {
	action := a.Action
	if action.SessionID != "daemon" {
		found := false
		for _, target := range g.SessionTargets {
			if target != "daemon" && opaque(g, "s", target) == action.SessionID {
				action.SessionID = target
				found = true
				break
			}
		}
		if !found {
			return action, store.ErrControlForbidden
		}
	}
	refs := make([]domain.ResourceRef, 0, len(action.Params.MonitorResources))
	for _, ref := range action.Params.MonitorResources {
		resolved, found := domain.ResourceRef{}, false
		for _, account := range st.Accounts {
			if opaque(g, "a", account.ID) != ref.AccountID {
				continue
			}
			resolved.AccountID = account.ID
			windows := account.Windows
			if ref.GroupID != "" {
				for _, group := range account.Groups {
					if opaque(g, "grp", account.ID+"\x00"+group.ID) == ref.GroupID {
						resolved.GroupID = group.ID
						windows = group.Windows
						break
					}
				}
				if resolved.GroupID == "" {
					break
				}
			}
			if ref.WindowID != "" {
				for _, window := range windows {
					if window.ID != "" && opaque(g, "win", account.ID+"\x00"+resolved.GroupID+"\x00"+window.ID) == ref.WindowID {
						resolved.WindowID = window.ID
						break
					}
				}
				if resolved.WindowID == "" {
					break
				}
			}
			found = true
			break
		}
		if !found {
			return action, store.ErrControlInvalid
		}
		refs = append(refs, resolved)
	}
	action.Params.MonitorResources = refs
	return action, nil
}

func (s *Server) scopedAction(w http.ResponseWriter, r *http.Request, g store.ControlGrant) {
	var in localapi.ActionInput
	if !decodeControl(w, r, &in, 16<<10) {
		return
	}
	if in.TTLSeconds < 1 || in.TTLSeconds > 300 || !store.ValidControlID(in.RequestID) {
		controlError(w, store.ErrControlInvalid)
		return
	}
	var created store.ControlRequest
	err := s.controlStore.Update(r.Context(), func(t *store.Tx) error {
		// Resolve only verified inventory handles; clients cannot invent source mappings.
		action, err := resolveAction(t.State, g, in)
		if err != nil {
			return err
		}
		grant, ok := t.ControlGrants[g.ID]
		if !ok || !grant.Allows("read", "", time.Now().UTC()) {
			return store.ErrControlForbidden
		}
		now := time.Now().UTC()
		created, err = t.CreateControlRequest(store.ControlRequest{ID: in.RequestID, GrantID: g.ID, Action: action, ExpiresAt: now.Add(time.Duration(in.TTLSeconds) * time.Second)}, now)
		return err
	})
	if err != nil {
		controlError(w, err)
		return
	}
	writeJSON(w, 202, localapi.RequestResponse{Request: scopedRequest(requestView(created), g)})
}
