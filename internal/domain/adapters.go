package domain

import "time"

const DefaultSourceID = "cpa-default"

// AdapterDescriptor describes implemented interfaces, not inferred product integrations.
type AdapterDescriptor struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Implemented  bool     `json:"implemented"`
	ReadOnly     bool     `json:"read_only"`
	Capabilities []string `json:"capabilities"`
	Limitations  []string `json:"limitations"`
}

// SourceHealth is independent of desktop and session-observer availability.
// CheckedAt is the last attempt; LastSuccessAt never advances on a failed attempt.
type SourceHealth struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	State         string     `json:"state"`
	Connected     bool       `json:"connected"`
	CheckedAt     time.Time  `json:"checked_at"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	Error         string     `json:"error,omitempty"`
	AccountCount  int        `json:"account_count"`
}

// AccountID preserves all legacy CPA IDs and namespaces every other source instance.
func AccountID(sourceID, rawID string) string {
	if sourceID == "" || sourceID == DefaultSourceID {
		return rawID
	}
	return sourceID + "::" + rawID
}

// SourceConnected never lets another source's health authorize this account.
func SourceConnected(a Account, caps Capabilities) bool {
	id := a.SourceID
	if id == "" {
		id = DefaultSourceID
	}
	for _, health := range caps.Sources {
		if health.ID == id {
			return health.Connected && health.State == "healthy"
		}
	}
	// Old sources implement only the legacy single-source capability interface.
	return len(caps.Sources) == 0 && id == DefaultSourceID && caps.ProxyConnected
}
