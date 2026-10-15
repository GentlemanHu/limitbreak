package store

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"quota-session-orchestrator/internal/domain"
)

const (
	ControlPending     = "pending"
	ControlApproved    = "approved"
	ControlRejected    = "rejected"
	ControlExecuting   = "executing"
	ControlCompleted   = "completed"
	ControlUncertain   = "uncertain"
	MaxControlGrants   = 128
	MaxControlRequests = 512
)

var (
	ErrControlConflict  = errors.New("control conflict")
	ErrControlForbidden = errors.New("control forbidden")
	ErrControlExpired   = errors.New("control expired")
	ErrControlLimit     = errors.New("control record limit")
	ErrControlInvalid   = errors.New("invalid control")
)

// TokenHash is persisted, but never returned by HTTP. The secret exists only at issuance.
type ControlGrant struct {
	ID             string     `json:"id"`
	TokenHash      string     `json:"token_hash"`
	Scopes         []string   `json:"scopes"`
	SessionTargets []string   `json:"session_targets"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

type ControlParams struct {
	MonitorResources []domain.ResourceRef       `json:"monitor_resources,omitempty"`
	MonitorMode      string                     `json:"monitor_mode,omitempty"`
	Enabled          *bool                      `json:"enabled,omitempty"`
	SettingsPatch    map[string]json.RawMessage `json:"settings_patch,omitempty"`
	// Per-session controls.
	SessionSettings *domain.SessionSettings `json:"session_settings,omitempty"`
	Message         string                  `json:"message,omitempty"`
}

type ControlAction struct {
	Operation          string        `json:"operation"`
	SessionID          string        `json:"session_id"`
	ExpectedGeneration int64         `json:"expected_generation"`
	PolicyDigest       string        `json:"policy_digest,omitempty"`
	Params             ControlParams `json:"params"`
}

type ControlResult struct {
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	Generation int64  `json:"generation,omitempty"`
	// Cancel cannot prove a send already handed to the desktop was retracted.
	SendRetractionConfirmed bool `json:"send_retraction_confirmed"`
}

type ControlRequest struct {
	ID          string         `json:"id"`
	GrantID     string         `json:"grant_id"`
	Action      ControlAction  `json:"action"`
	Digest      string         `json:"digest"`
	State       string         `json:"state"`
	CreatedAt   time.Time      `json:"created_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	ApprovedAt  *time.Time     `json:"approved_at,omitempty"`
	ExecutingAt *time.Time     `json:"executing_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	Result      *ControlResult `json:"result,omitempty"`
}

func ControlTokenHash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func ControlDigest(action ControlAction) string {
	b, _ := json.Marshal(action)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func PolicyDigest(policy domain.Policy) string {
	b, _ := json.Marshal(policy)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func ValidControlID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if c < 33 || c > 126 || strings.ContainsRune("/\\\"", c) {
			return false
		}
	}
	return true
}

// Resource identities are data from verified inventories, never paths to open.
// They need not use the narrower syntax of control/request identifiers.
func validControlResourceID(id string, limit int) bool {
	return id != "" && len(id) <= limit && utf8.ValidString(id) && strings.TrimSpace(id) == id && strings.IndexFunc(id, unicode.IsControl) < 0
}

func ControlScope(operation string) string { return "action:" + operation }

func ValidControlScope(scope string) bool {
	if scope == "read" {
		return true
	}
	switch strings.TrimPrefix(scope, "action:") {
	case "refresh", "cancel", "pause", "unpause", "bind_monitor", "enable_auto_resume", "settingsPatch", "session_settings", "test_send", "locate":
		return strings.HasPrefix(scope, "action:")
	}
	return false
}

func (g ControlGrant) Allows(scope, target string, now time.Time) bool {
	if g.RevokedAt != nil || !now.Before(g.ExpiresAt) || now.Before(g.CreatedAt) {
		return false
	}
	found := false
	for _, s := range g.Scopes {
		if s == scope {
			found = true
		}
	}
	if !found {
		return false
	}
	if target == "" {
		return true
	}
	for _, s := range g.SessionTargets {
		if s == target {
			return true
		}
	}
	return false
}

func (s *State) FindControlGrant(secret string, now time.Time) (ControlGrant, bool) {
	if !strings.HasPrefix(secret, "qso_scope_") || len(secret) != len("qso_scope_")+64 {
		return ControlGrant{}, false
	}
	h := ControlTokenHash(secret)
	for _, g := range s.ControlGrants {
		if subtle.ConstantTimeCompare([]byte(g.TokenHash), []byte(h)) == 1 && g.Allows("read", "", now) {
			return g, true
		}
	}
	return ControlGrant{}, false
}

func ValidateControlAction(a ControlAction) error {
	if !ValidControlID(a.SessionID) || a.ExpectedGeneration < 0 || !ValidControlScope(ControlScope(a.Operation)) {
		return ErrControlInvalid
	}
	global := a.Operation == "refresh" || a.Operation == "pause" || a.Operation == "unpause" || a.Operation == "settingsPatch"
	if global != (a.SessionID == "daemon") || global && a.ExpectedGeneration != 0 {
		return ErrControlInvalid
	}
	p := a.Params
	if a.Operation != "bind_monitor" && (len(p.MonitorResources) != 0 || p.MonitorMode != "") {
		return ErrControlInvalid
	}
	if a.Operation != "enable_auto_resume" && p.Enabled != nil {
		return ErrControlInvalid
	}
	if a.Operation != "settingsPatch" && len(p.SettingsPatch) != 0 {
		return ErrControlInvalid
	}
	if a.Operation == "enable_auto_resume" && p.Enabled == nil {
		return ErrControlInvalid
	}
	if (a.Operation == "session_settings") != (p.SessionSettings != nil) || a.Operation != "test_send" && p.Message != "" {
		return ErrControlInvalid
	}
	if p.SessionSettings != nil {
		ss := *p.SessionSettings
		if ss == (domain.SessionSettings{}) || domain.ValidateSessionSettings(ss) != nil {
			return ErrControlInvalid
		}
	}
	if p.Message != "" && !domain.ValidContinueMessage(p.Message) {
		return ErrControlInvalid
	}
	if a.Operation == "bind_monitor" {
		if len(p.MonitorResources) == 0 || len(p.MonitorResources) > 64 || p.MonitorMode != "any" && p.MonitorMode != "all" {
			return ErrControlInvalid
		}
		seen := map[domain.ResourceRef]bool{}
		for _, ref := range p.MonitorResources {
			if !validControlResourceID(ref.AccountID, 512) || ref.GroupID != "" && !validControlResourceID(ref.GroupID, 128) || ref.WindowID != "" && !validControlResourceID(ref.WindowID, 128) || seen[ref] {
				return ErrControlInvalid
			}
			seen[ref] = true
		}
	}
	if a.Operation == "pause" || a.Operation == "unpause" || a.Operation == "settingsPatch" {
		if len(a.PolicyDigest) != 64 {
			return ErrControlInvalid
		}
		if _, err := hex.DecodeString(a.PolicyDigest); err != nil {
			return ErrControlInvalid
		}
	} else if a.PolicyDigest != "" {
		return ErrControlInvalid
	}
	if a.Operation == "settingsPatch" {
		if len(p.SettingsPatch) == 0 || len(p.SettingsPatch) > 16 {
			return ErrControlInvalid
		}
		for key, value := range p.SettingsPatch {
			switch key {
			case "max_running", "max_per_account", "safety_seconds", "freshness_seconds", "verify_seconds", "long_probe_seconds", "unknown_probe_seconds", "min_send_interval_seconds", "early_probe_seconds", "near_reset_probe_seconds", "reset_lookahead_seconds", "user_idle_seconds", "running_stale_seconds", "queue_aging_seconds", "max_auto_resumes_per_hour":
			default:
				return ErrControlInvalid
			}
			var v int
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &v) != nil || v < 0 || v > 86400 {
				return ErrControlInvalid
			}
		}
	}
	return nil
}

func (t *Tx) InitControl() {
	if t.ControlGrants == nil {
		t.ControlGrants = map[string]ControlGrant{}
	}
	if t.ControlRequests == nil {
		t.ControlRequests = map[string]ControlRequest{}
	}
}

// Expiration rejects untouched requests. Abandoned claims are uncertain, never retried.
// Tombstones are retained until the grant has expired so idempotency survives retries.
func (t *Tx) PruneControl(now time.Time) {
	t.InitControl()
	for id, r := range t.ControlRequests {
		if !now.Before(r.ExpiresAt) {
			switch r.State {
			case ControlPending, ControlApproved:
				r.State = ControlRejected
				r.Result = &ControlResult{Code: "expired"}
				r.CompletedAt = &now
			case ControlExecuting:
				r.State = ControlUncertain
				r.Result = &ControlResult{Code: "execution_uncertain"}
				r.CompletedAt = &now
			}
			t.ControlRequests[id] = r
		}
		g, ok := t.ControlGrants[r.GrantID]
		if !ok || now.After(g.ExpiresAt.Add(24*time.Hour)) {
			delete(t.ControlRequests, id)
		}
	}
	for id, g := range t.ControlGrants {
		if now.After(g.ExpiresAt.Add(24 * time.Hour)) {
			delete(t.ControlGrants, id)
		}
	}
}

func (t *Tx) AddControlGrant(g ControlGrant, now time.Time) error {
	t.PruneControl(now)
	if len(t.ControlGrants) >= MaxControlGrants {
		return ErrControlLimit
	}
	if !ValidControlID(g.ID) || len(g.TokenHash) != 64 || g.ExpiresAt.Sub(now) < time.Minute || g.ExpiresAt.Sub(now) > 24*time.Hour || len(g.Scopes) > 8 || len(g.SessionTargets) == 0 || len(g.SessionTargets) > 128 {
		return ErrControlInvalid
	}
	if _, exists := t.ControlGrants[g.ID]; exists {
		return ErrControlConflict
	}
	read := false
	seen := map[string]bool{}
	for _, scope := range g.Scopes {
		if !ValidControlScope(scope) || seen[scope] {
			return ErrControlInvalid
		}
		seen[scope] = true
		read = read || scope == "read"
	}
	if !read {
		return ErrControlInvalid
	}
	seen = map[string]bool{}
	for _, target := range g.SessionTargets {
		if !ValidControlID(target) || seen[target] {
			return ErrControlInvalid
		}
		seen[target] = true
		if target != "daemon" {
			if _, ok := t.Sessions[target]; !ok {
				return ErrControlInvalid
			}
		}
	}
	g.CreatedAt = now
	t.ControlGrants[g.ID] = g
	return nil
}

func (t *Tx) RevokeControlGrant(id string, now time.Time) error {
	t.InitControl()
	g, ok := t.ControlGrants[id]
	if !ok {
		return ErrControlInvalid
	}
	if g.RevokedAt == nil {
		g.RevokedAt = &now
		t.ControlGrants[id] = g
	}
	for id, r := range t.ControlRequests {
		if r.GrantID != g.ID {
			continue
		}
		if r.State == ControlPending || r.State == ControlApproved {
			r.State = ControlRejected
			r.Result = &ControlResult{Code: "grant_revoked"}
			r.CompletedAt = &now
			t.ControlRequests[id] = r
		}
	}
	return nil
}

func (t *Tx) CheckControlAction(g ControlGrant, a ControlAction, now time.Time) error {
	if err := ValidateControlAction(a); err != nil {
		return err
	}
	if !g.Allows(ControlScope(a.Operation), a.SessionID, now) {
		return ErrControlForbidden
	}
	if a.SessionID != "daemon" {
		s, ok := t.Sessions[a.SessionID]
		if !ok || s.Generation != a.ExpectedGeneration {
			return ErrControlConflict
		}
	}
	if a.PolicyDigest != "" && (t.Policy == nil || PolicyDigest(*t.Policy) != a.PolicyDigest) {
		return ErrControlConflict
	}
	return nil
}

func (t *Tx) CreateControlRequest(r ControlRequest, now time.Time) (ControlRequest, error) {
	t.PruneControl(now)
	if old, ok := t.ControlRequests[r.ID]; ok {
		if old.GrantID != r.GrantID || old.Digest != ControlDigest(r.Action) {
			return ControlRequest{}, ErrControlConflict
		}
		return old, nil
	}
	if len(t.ControlRequests) >= MaxControlRequests {
		return ControlRequest{}, ErrControlLimit
	}
	if !ValidControlID(r.ID) || !now.Before(r.ExpiresAt) || r.ExpiresAt.Sub(now) > 5*time.Minute {
		return ControlRequest{}, ErrControlInvalid
	}
	g, ok := t.ControlGrants[r.GrantID]
	if !ok {
		return ControlRequest{}, ErrControlForbidden
	}
	if err := t.CheckControlAction(g, r.Action, now); err != nil {
		return ControlRequest{}, err
	}
	if r.ExpiresAt.After(g.ExpiresAt) {
		r.ExpiresAt = g.ExpiresAt
	}
	r.Digest = ControlDigest(r.Action)
	r.State = ControlPending
	r.CreatedAt = now
	r.ApprovedAt = nil
	r.ExecutingAt = nil
	r.CompletedAt = nil
	r.Result = nil
	t.ControlRequests[r.ID] = r
	return r, nil
}

func (t *Tx) ApproveControl(id, digest string, generation int64, approve bool, now time.Time) (ControlRequest, error) {
	t.InitControl()
	r, ok := t.ControlRequests[id]
	if !ok {
		return ControlRequest{}, ErrControlInvalid
	}
	if r.Digest != digest || r.Action.ExpectedGeneration != generation {
		return ControlRequest{}, ErrControlConflict
	}
	if r.State != ControlPending {
		return r, nil
	}
	if !now.Before(r.ExpiresAt) {
		return ControlRequest{}, ErrControlExpired
	}
	g, ok := t.ControlGrants[r.GrantID]
	if !ok {
		return ControlRequest{}, ErrControlForbidden
	}
	if err := t.CheckControlAction(g, r.Action, now); err != nil {
		return ControlRequest{}, err
	}
	if approve {
		r.State = ControlApproved
		r.ApprovedAt = &now
	} else {
		r.State = ControlRejected
		r.CompletedAt = &now
		r.Result = &ControlResult{Code: "user_rejected"}
	}
	t.ControlRequests[id] = r
	return r, nil
}

// ClaimControl MUST be called inside the Engine's own transaction. Never nest Update.
func (t *Tx) ClaimControl(id, digest string, now time.Time) (ControlRequest, bool, error) {
	t.InitControl()
	r, ok := t.ControlRequests[id]
	if !ok {
		return ControlRequest{}, false, ErrControlInvalid
	}
	if r.Digest != digest || r.Digest != ControlDigest(r.Action) {
		return r, false, ErrControlConflict
	}
	if r.State != ControlApproved {
		return r, false, nil
	}
	if !now.Before(r.ExpiresAt) {
		return r, false, ErrControlExpired
	}
	g, ok := t.ControlGrants[r.GrantID]
	if !ok {
		return r, false, ErrControlForbidden
	}
	if err := t.CheckControlAction(g, r.Action, now); err != nil {
		return r, false, err
	}
	r.State = ControlExecuting
	r.ExecutingAt = &now
	t.ControlRequests[id] = r
	return r, true, nil
}

func (t *Tx) FinishControl(id, digest string, result ControlResult, now time.Time) error {
	t.InitControl()
	r, ok := t.ControlRequests[id]
	if !ok || r.Digest != digest {
		return ErrControlConflict
	}
	if r.State != ControlExecuting {
		return ErrControlConflict
	}
	if result.Code == "" || len(result.Code) > 64 {
		return ErrControlInvalid
	}
	result.SendRetractionConfirmed = false
	r.State = ControlCompleted
	if result.Code == "execution_uncertain" {
		r.State = ControlUncertain
	}
	r.Result = &result
	r.CompletedAt = &now
	t.ControlRequests[id] = r
	return nil
}

func SortedControlRequestIDs(s *State) []string {
	ids := make([]string, 0, len(s.ControlRequests))
	for id := range s.ControlRequests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
