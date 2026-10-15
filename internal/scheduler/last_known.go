package scheduler

import "quota-session-orchestrator/internal/domain"

// Persisted last-known display data survives a process restart, but never gains a new evidence timestamp.
func keepLastKnown(incoming, previous []domain.Account) {
	prior := map[string]domain.Account{}
	for _, a := range previous {
		if a.SourceID == "" {
			a.SourceID = domain.DefaultSourceID
		}
		prior[a.ID] = a
	}
	for i := range incoming {
		a := &incoming[i]
		old, ok := prior[a.ID]
		if !ok || a.QuotaError == "" || len(a.Groups) > 0 || a.SourceID != old.SourceID || a.AuthIndex != old.AuthIndex || a.Provider != old.Provider || a.Email != old.Email {
			continue
		}
		a.Groups = append([]domain.QuotaGroup(nil), old.Groups...)
		for n := range a.Groups {
			a.Groups[n].State = "unknown"
		}
		a.Windows = old.Windows
		a.Metrics = old.Metrics
		a.CheckedAt = old.CheckedAt
		if a.QuotaSource == "" {
			a.QuotaSource = old.QuotaSource
		}
		if a.Plan == "" {
			a.Plan = old.Plan
		}
		if !a.Disabled {
			a.State = "unknown"
		}
	}
}
