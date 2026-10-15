package adapters

import (
	"quota-session-orchestrator/internal/domain"
	"time"
)

// Last observed identity sets are separate from last-known display data. A
// rejected collision must remain detectable while one participant is not due,
// including first-poll collisions that have no earlier display data to retain.
func (a *Aggregator) quarantineCollisions(previous map[string][]domain.Account, previousHealth map[string]domain.SourceHealth) map[string]bool {
	owners := map[string]map[string]bool{}
	add := func(id, source string) {
		if owners[id] == nil {
			owners[id] = map[string]bool{}
		}
		owners[id][source] = true
	}
	for _, source := range a.instances {
		for _, id := range a.observed[source.ID] {
			add(id, source.ID)
		}
		// Retained identities also block borrowing when a failed list is incomplete.
		for _, account := range a.accounts[source.ID] {
			add(account.ID, source.ID)
		}
	}
	ambiguous := map[string]bool{}
	affected := map[string]bool{}
	for id, sources := range owners {
		if len(sources) < 2 {
			continue
		}
		ambiguous[id] = true
		for source := range sources {
			affected[source] = true
		}
	}
	for source := range affected {
		health := a.health[source]
		health.Connected = false
		health.State = "error"
		health.Error = "账号 ID 命名空间冲突；受影响来源已隔离，歧义账号不能授权发送"
		health.LastSuccessAt = cloneTime(previousHealth[source].LastSuccessAt)
		a.health[source] = health
		a.accounts[source] = cloneAccounts(previous[source])
		markUnknown(a.accounts[source], source, health.Error)
	}
	// Recheck the rolled-back display set too: persisted legacy records must
	// never get merged by ID or overwrite a different source's retained account.
	counts := map[string]int{}
	for _, source := range a.instances {
		for _, account := range a.accounts[source.ID] {
			counts[account.ID]++
		}
	}
	for id, count := range counts {
		if count > 1 {
			ambiguous[id] = true
		}
	}
	return ambiguous
}

func markUnknown(accounts []domain.Account, source, reason string) {
	for i := range accounts {
		a := &accounts[i]
		a.SourceID = source
		a.QuotaError = reason
		a.Reason = reason
		a.State = "unknown"
		if a.Disabled {
			a.State = "disabled"
		}
		for n := range a.Groups {
			a.Groups[n].State = "unknown"
		}
	}
}

// NextRefreshAt lets the production timer honor source cadence even when a
// source returns an empty accounts list. Reading the schedule never refreshes evidence.
func (a *Aggregator) NextRefreshAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	var next time.Time
	for _, source := range a.instances {
		due := a.nextDue[source.ID]
		if due.IsZero() {
			return time.Time{}
		}
		if next.IsZero() || due.Before(next) {
			next = due
		}
	}
	return next
}
