package scheduler

import (
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

func canonicalSource(id string) string {
	if id == "" {
		return domain.DefaultSourceID
	}
	return id
}

func singleDefaultCPA(sources []domain.SourceHealth) bool {
	return len(sources) == 1 && sources[0].ID == domain.DefaultSourceID && sources[0].Type == "cliproxyapi"
}

// Only the engine derives affinity from its own verified inventory. The HTTP
// binding contract has no account-to-source map and unknown references remain
// valid for monitoring, but cannot authorize a multi-source resume.
func (e *Engine) captureSources(t *store.Tx, b domain.Binding) map[string]string {
	result := map[string]string{}
	for _, id := range b.AccountIDs {
		expected := ""
		found, ambiguous := false, false
		known := ""
		for _, account := range t.Accounts {
			if account.ID != id {
				continue
			}
			if found {
				ambiguous = true
				break
			}
			found = true
			known = canonicalSource(account.SourceID)
			if domain.SourceConnected(account, t.Capabilities) {
				expected = canonicalSource(account.SourceID)
			}
		}
		if ambiguous {
			expected = ""
		}
		// Legacy v2 bindings can be enrolled before the first quota refresh.
		// This fallback is limited to a real single-default-CPA configuration;
		// never infer source identity from names, email or a textual ID prefix.
		if expected == "" && !ambiguous && (!found || known == domain.DefaultSourceID) && singleDefaultCPA(e.initialSources) {
			expected = domain.DefaultSourceID
		}
		result[id] = expected
	}
	return result
}

func (e *Engine) migrateSources(t *store.Tx) {
	if t.BoundSources == nil {
		t.BoundSources = map[string]map[string]string{}
	}
	for id, session := range t.Sessions {
		if session.Binding == nil || !session.Binding.AutoResume {
			continue
		}
		if _, exists := t.BoundSources[id]; !exists {
			// Retained multi-source inventory does not prove an old binding's owner.
			// Persist unresolved affinity so later refreshes cannot auto-enroll it.
			unresolved := map[string]string{}
			for _, accountID := range session.Binding.AccountIDs {
				unresolved[accountID] = ""
			}
			if singleDefaultCPA(e.initialSources) && (len(t.Capabilities.Sources) == 0 || singleDefaultCPA(t.Capabilities.Sources)) {
				unresolved = e.captureSources(t, *session.Binding)
			}
			t.BoundSources[id] = unresolved
		}
	}
}

func boundSourceMatches(t *store.Tx, sessionID string, account domain.Account) bool {
	expected := t.BoundSources[sessionID][account.ID]
	return expected != "" && expected == canonicalSource(account.SourceID)
}
