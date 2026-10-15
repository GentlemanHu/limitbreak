package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// The app edits a narrow patch against the exact policy it displayed. A stale
// snapshot is rejected instead of replacing unrelated settings.
func (s *Server) policyPatch(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "POST") {
		return
	}
	var in struct {
		PolicyDigest string                     `json:"policy_digest"`
		Patch        map[string]json.RawMessage `json:"patch"`
	}
	if !decodeControl(w, r, &in, 16<<10) {
		return
	}
	if len(in.PolicyDigest) != 64 {
		controlError(w, store.ErrControlInvalid)
		return
	}
	if _, err := hex.DecodeString(in.PolicyDigest); err != nil {
		controlError(w, store.ErrControlInvalid)
		return
	}
	editor, ok := s.engine.(interface {
		PatchPolicy(context.Context, string, map[string]json.RawMessage) (domain.Policy, error)
	})
	if !ok {
		fail(w, 503, "policy_patch_unavailable", "当前引擎不支持原子策略修改")
		return
	}
	p, err := editor.PatchPolicy(r.Context(), in.PolicyDigest, in.Patch)
	if err != nil {
		controlError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "policy": p, "policy_digest": store.PolicyDigest(p)})
}
