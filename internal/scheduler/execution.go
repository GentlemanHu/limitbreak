package scheduler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

// ValidateExecution authorizes no action itself. The App calls it again directly
// before every desktop write, using the engine-owned lease and effective text.
func (e *Engine) ValidateExecution(ctx context.Context, sessionID string, generation int64, leaseID, messageHash string) (bool, error) {
	if e.store == nil || sessionID == "" || generation <= 0 || len(leaseID) > 512 || len(messageHash) != 64 {
		return false, errors.New("invalid desktop execution check")
	}
	digest, err := hex.DecodeString(messageHash)
	if err != nil || len(digest) != sha256.Size {
		return false, errors.New("invalid message digest")
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	state, err := e.store.Read(ctx)
	if err != nil {
		return false, err
	}
	now := e.now()
	if state.Policy == nil || leaseID == "" || state.DesktopOwner != leaseID || !state.DesktopUntil.After(now) {
		return false, nil
	}
	session, ok := state.Sessions[sessionID]
	if !ok || session.Generation != generation || session.State != domain.Dispatching {
		return false, nil
	}
	var job *domain.Job
	for _, value := range state.Jobs {
		if value.SessionID == sessionID && value.Generation == generation && value.State == domain.Dispatching && leaseID == e.owner+"/"+value.ID {
			copy := value
			if job != nil {
				return false, nil
			}
			job = &copy
		}
	}
	if job == nil || job.LeaseUntil == nil || !job.LeaseUntil.After(now) || (job.Kind != "test" && state.Policy.Paused) {
		return false, nil
	}
	policy := config.EffectivePolicy(*state.Policy)
	current, problem := jobBinding(state.Accounts, session, *job, policy)
	if problem != "" || domain.ValidateResumeFields(current) != nil {
		return false, nil
	}
	if job.Kind != "test" && current.ResumeOwner != "guard" && current.ResumeOwner != "auto" {
		return false, nil
	}
	expected := sha256.Sum256([]byte(current.ContinueMessage))
	if job.MessageDigest != hex.EncodeToString(expected[:]) || subtle.ConstantTimeCompare(expected[:], digest) != 1 {
		return false, nil
	}
	if job.Kind == "test" {
		return true, nil
	}
	if !contains(current.AccountIDs, job.AccountID) {
		return false, nil
	}
	tx := &store.Tx{State: state}
	var account *domain.Account
	for _, value := range state.Accounts {
		if value.ID == job.AccountID {
			if account != nil {
				return false, nil
			}
			copy := value
			account = &copy
		}
	}
	if account == nil || !domain.SourceConnected(*account, state.Capabilities) || (current.ResumeOwner == "guard" && !boundSourceMatches(tx, sessionID, *account)) {
		return false, nil
	}
	if ok, _ := quotaEligible(*account, current, policy, state.QuotaFailures[sessionID], now); !ok {
		return false, nil
	}
	return true, nil
}
