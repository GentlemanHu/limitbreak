package localapi

import (
	"encoding/hex"
	"math"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// ValidHandle verifies the daemon's format without remapping or decoding identity.
func ValidHandle(id, kind string) bool {
	prefix := kind + "_"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+48 {
		return false
	}
	return lowerHex(strings.TrimPrefix(id, prefix), 48)
}

func lowerHex(s string, length int) bool {
	if len(s) != length || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func ValidateActionInput(in ActionInput) error {
	if !store.ValidControlID(in.RequestID) || in.TTLSeconds < 1 || in.TTLSeconds > 300 || store.ValidateControlAction(in.Action) != nil || !scopedAction(in.Action) {
		return ErrDaemon
	}
	return nil
}

func scopedAction(a store.ControlAction) bool {
	if a.SessionID != "daemon" && !ValidHandle(a.SessionID, "s") {
		return false
	}
	for _, ref := range a.Params.MonitorResources {
		if !ValidHandle(ref.AccountID, "a") || ref.GroupID != "" && !ValidHandle(ref.GroupID, "grp") || ref.WindowID != "" && !ValidHandle(ref.WindowID, "win") {
			return false
		}
	}
	return a.PolicyDigest == "" || lowerHex(a.PolicyDigest, 64)
}

func ValidateStatus(s Status) error {
	if !lowerHex(s.PolicyDigest, 64) || !s.ControlRequiresConfirmation || s.Sessions < 0 || s.Jobs < 0 || s.Accounts < 0 {
		return ErrDaemon
	}
	return nil
}

func validState(s string) bool {
	switch s {
	case "", domain.Running, domain.Completed, domain.Cooldown, domain.Ready, domain.Dispatching, domain.Verifying, domain.Attention, domain.Cancelled,
		"healthy", "unknown", "degraded", "unavailable", "allowed", "limited", "stale", "disabled", "missing", "error", "active", "connected", "disconnected":
		return true
	}
	return false
}

func validKind(s string) bool {
	switch s {
	case "", "unknown", "code", "chat", "cowork", "cliproxyapi", "readonly-http", "antigravity", "quotio", "executor", "observer", "quota", "quota_source", "session_observer", "resume_executor", "reset_signal":
		return true
	}
	return false
}

func ValidatePage(p Page, collection string, limit int) error {
	if limit < 1 || limit > 100 || len(p.Items) > limit || len(p.NextCursor) > 128 {
		return ErrDaemon
	}
	kinds := map[string][]string{"resources": {"a", "grp", "win"}, "sources": {"src"}, "sessions": {"s"}, "queue": {"j"}, "adapters": {"adapter"}, "diagnostics": {"diag"}}
	allowed, ok := kinds[collection]
	if !ok {
		return ErrDaemon
	}
	seen := map[string]bool{}
	for _, item := range p.Items {
		idOK := false
		for _, kind := range allowed {
			idOK = idOK || ValidHandle(item.ID, kind)
		}
		if !idOK || seen[item.ID] || !validState(item.State) || !validKind(item.Kind) || item.Generation < 0 {
			return ErrDaemon
		}
		seen[item.ID] = true
		for _, v := range []struct{ id, kind string }{{item.SessionID, "s"}, {item.SourceID, "src"}, {item.AccountID, "a"}, {item.GroupID, "grp"}, {item.WindowID, "win"}} {
			if v.id != "" && !ValidHandle(v.id, v.kind) {
				return ErrDaemon
			}
		}
		if item.UsedPercent != nil && (math.IsNaN(*item.UsedPercent) || math.IsInf(*item.UsedPercent, 0) || *item.UsedPercent < 0 || *item.UsedPercent > 100) {
			return ErrDaemon
		}
	}
	if p.NextCursor != "" && (len(p.Items) == 0 || p.NextCursor != p.Items[len(p.Items)-1].ID) {
		return ErrDaemon
	}
	return nil
}

func ValidateRequest(r RequestView) error {
	if !store.ValidControlID(r.ID) || len(r.GrantID) > 256 || !lowerHex(r.Digest, 64) || store.ValidateControlAction(r.Action) != nil || !scopedAction(r.Action) || !r.ExpiresAt.After(r.CreatedAt) || r.ExpiresAt.Sub(r.CreatedAt) > 300*time.Second {
		return ErrDaemon
	}
	switch r.State {
	case store.ControlPending, store.ControlApproved, store.ControlRejected, store.ControlExecuting, store.ControlCompleted, store.ControlUncertain:
	default:
		return ErrDaemon
	}
	if r.Result != nil {
		if r.Result.Generation < 0 || r.Result.SendRetractionConfirmed {
			return ErrDaemon
		}
		switch r.Result.Code {
		case "completed", "control_rejected", "not_executed", "refresh_completed", "refresh_failed", "execution_uncertain", "expired", "grant_revoked", "user_rejected":
		default:
			return ErrDaemon
		}
	}
	return nil
}

func ValidateRequests(p RequestPage, limit int) error {
	if limit < 1 || limit > 100 || len(p.Requests) > limit || len(p.NextCursor) > 128 {
		return ErrDaemon
	}
	seen := map[string]bool{}
	for _, r := range p.Requests {
		if ValidateRequest(r) != nil || seen[r.ID] {
			return ErrDaemon
		}
		seen[r.ID] = true
	}
	if p.NextCursor != "" && (len(p.Requests) == 0 || p.NextCursor != p.Requests[len(p.Requests)-1].ID) {
		return ErrDaemon
	}
	return nil
}
