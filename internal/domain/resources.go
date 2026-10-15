package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"time"
)

// Group scope is independent from account routing. Unknown model scope is never inferred from a label.
type QuotaGroup struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Scope       string     `json:"scope"`
	Models      []string   `json:"models,omitempty"`
	Windows     []Window   `json:"windows"`
	State       string     `json:"state"`
	CheckedAt   time.Time  `json:"checked_at"`
	AvailableAt *time.Time `json:"available_at,omitempty"`
}

type QuotaMetric struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Value    string `json:"value"`
	Unit     string `json:"unit,omitempty"`
	Format   string `json:"format,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// Empty GroupID selects account-level windows. Empty WindowID selects the whole
// account/group; a nonempty WindowID narrows monitoring only, never resume authority.
type ResourceRef struct {
	AccountID string `json:"account_id"`
	GroupID   string `json:"group_id,omitempty"`
	WindowID  string `json:"window_id,omitempty"`
}

type ResourceStatus struct {
	Resource    ResourceRef `json:"resource"`
	Label       string      `json:"label"`
	State       string      `json:"state"`
	Reason      string      `json:"reason"`
	AvailableAt *time.Time  `json:"available_at,omitempty"`
}

type MonitorStatus struct {
	Mode        string           `json:"mode"`
	State       string           `json:"state"`
	Resources   []ResourceStatus `json:"resources"`
	AvailableAt *time.Time       `json:"available_at,omitempty"`
}

func ResourceID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// modelFamilies are the family tokens a provider may scope a quota group to.
var modelFamilies = []string{"opus", "sonnet", "haiku"}

// GroupApplies reports whether a quota group limits requests for model. It is
// conservative: account and unknown scopes always apply, an operation scope
// (such as code review) never limits a conversation, and a model scope applies
// unless the model is known and clearly outside it. An empty model, or one
// naming no family, is treated as possibly in every model scope.
func GroupApplies(g QuotaGroup, model string) bool {
	switch g.Scope {
	case "operation":
		return false
	case "models":
	default:
		return true
	}
	m := NormalizeModel(model)
	if m == "" || len(g.Models) == 0 {
		return true
	}
	family := ""
	for _, f := range modelFamilies {
		if strings.Contains(m, f) {
			family = f
			break
		}
	}
	for _, scoped := range g.Models {
		s := NormalizeModel(scoped)
		if s == "" || s == m || strings.HasPrefix(m, s) || strings.HasPrefix(s, m) {
			return true
		}
		// Same family (any version), or a model naming no family at all.
		if family == "" || strings.Contains(s, family) {
			return true
		}
	}
	return false
}

func WindowState(windows []Window) (string, *time.Time) {
	if len(windows) == 0 {
		return "unknown", nil
	}
	blocked, unknown := false, false
	var latest *time.Time
	for _, w := range windows {
		if w.Blocked || (w.UsedPercent != nil && *w.UsedPercent >= 100) {
			blocked = true
			if w.ResetAt == nil {
				unknown = true
			} else if latest == nil || w.ResetAt.After(*latest) {
				t := *w.ResetAt
				latest = &t
			}
		} else if w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 {
			unknown = true
		}
	}
	if blocked {
		if unknown {
			latest = nil
		}
		return "limited", latest
	}
	if unknown {
		return "unknown", nil
	}
	return "allowed", nil
}

func ObserveResources(refs []ResourceRef, mode string, accounts []Account, now time.Time, freshness time.Duration) *MonitorStatus {
	if len(refs) == 0 {
		return nil
	}
	if mode != "all" {
		mode = "any"
	}
	result := &MonitorStatus{Mode: mode, State: "unknown", Resources: []ResourceStatus{}}
	allowed, limited, unknown := 0, 0, 0
	unknownReset := false
	var next *time.Time
	for _, ref := range refs {
		st := observeResource(ref, accounts, now, freshness)
		result.Resources = append(result.Resources, st)
		switch st.State {
		case "allowed":
			allowed++
		case "limited":
			limited++
			if st.AvailableAt == nil {
				unknownReset = true
			}
		default:
			unknown++
		}
		if st.AvailableAt != nil && (next == nil || mode == "all" && st.AvailableAt.After(*next) || mode == "any" && st.AvailableAt.Before(*next)) {
			t := *st.AvailableAt
			next = &t
		}
	}
	if mode == "all" {
		if allowed == len(refs) {
			result.State = "allowed"
		} else if limited > 0 {
			result.State = "limited"
		}
	} else {
		if allowed > 0 {
			result.State = "allowed"
		} else if limited == len(refs) {
			result.State = "limited"
		}
	}
	if unknown == 0 && result.State != "allowed" && !(mode == "all" && unknownReset) {
		result.AvailableAt = next
	}
	return result
}

func observeResource(ref ResourceRef, accounts []Account, now time.Time, freshness time.Duration) ResourceStatus {
	st := ResourceStatus{Resource: ref, Label: ref.AccountID, State: "missing", Reason: "资源已移除或尚未采集"}
	for _, a := range accounts {
		if a.ID != ref.AccountID {
			continue
		}
		st.Label = a.Provider + " · " + a.Email
		checked := a.CheckedAt
		windows := a.Windows
		if ref.GroupID != "" {
			found := false
			for _, g := range a.Groups {
				if g.ID == ref.GroupID {
					st.Label += " / " + g.Name
					windows = g.Windows
					checked = g.CheckedAt
					found = true
					break
				}
			}
			if !found {
				return st
			}
		}
		if ref.WindowID != "" {
			found := false
			for _, w := range windows {
				if w.ID == ref.WindowID {
					st.Label += " / " + w.Name
					windows = []Window{w}
					found = true
					break
				}
			}
			if !found {
				return st
			}
		}
		if a.Disabled {
			st.State = "disabled"
			st.Reason = "凭证已停用；保留监控但不授权恢复"
			return st
		}
		if checked.IsZero() || checked.After(now.Add(5*time.Second)) || now.Sub(checked) > freshness {
			st.State = "stale"
			st.Reason = "额度证据过期或尚未核验"
			return st
		}
		st.State, st.AvailableAt = WindowState(windows)
		st.Reason = "仅监控所选资源；不代表实际路由或发送授权"
		if a.QuotaError != "" {
			st.State = "unknown"
			st.Reason = a.QuotaError
			st.AvailableAt = nil
		}
		return st
	}
	return st
}
