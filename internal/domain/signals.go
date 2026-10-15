package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Reset signal kinds. A signal is a hint that a provider's quota windows were
// (or will be) reset outside their normal schedule. It is never quota
// evidence: it only makes LimitBreak query that provider's accounts sooner.
// Every send still requires fresh evidence from a quota source.
const (
	SignalResetExecuted  = "reset_executed"
	SignalResetScheduled = "reset_scheduled"
)

type ResetSignal struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"`
	Provider   string    `json:"provider"`
	Kind       string    `json:"kind"`
	At         time.Time `json:"at"`
	ReceivedAt time.Time `json:"received_at"`
	Text       string    `json:"text,omitempty"`
	URL        string    `json:"url,omitempty"`
}

// ScheduledReset is an announced reset that has not happened yet. A passed
// time does not imply the reset happened.
type ScheduledReset struct {
	ID   string     `json:"id"`
	At   *time.Time `json:"at,omitempty"`
	Text string     `json:"text,omitempty"`
	URL  string     `json:"url,omitempty"`
}

// ResetWatch is a third-party forecast, never an official commitment.
type ResetWatch struct {
	Level         string     `json:"level"`
	ChancePercent *int       `json:"chance_percent,omitempty"`
	Window        string     `json:"window,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	Text          string     `json:"text,omitempty"`
	URL           string     `json:"url,omitempty"`
}

// SignalSourceState is the persisted, displayable state of one reset-signal
// source. ETag and LastID are bookkeeping for conditional polling and
// deduplication; neither is a secret.
type SignalSourceState struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Name            string          `json:"name"`
	Provider        string          `json:"provider"`
	Enabled         bool            `json:"enabled"`
	State           string          `json:"state"`
	Error           string          `json:"error,omitempty"`
	LastPollAt      *time.Time      `json:"last_poll_at,omitempty"`
	NextPollAt      *time.Time      `json:"next_poll_at,omitempty"`
	LastID          string          `json:"last_id,omitempty"`
	ETag            string          `json:"etag,omitempty"`
	Latest          *ResetSignal    `json:"latest,omitempty"`
	Scheduled       *ScheduledReset `json:"scheduled,omitempty"`
	Watch           *ResetWatch     `json:"watch,omitempty"`
	LastTriggeredAt *time.Time      `json:"last_triggered_at,omitempty"`
	Attribution     string          `json:"attribution,omitempty"`
	AttributionURL  string          `json:"attribution_url,omitempty"`
}

// ErrDuplicateSignal reports a pushed signal whose ID was already received.
var ErrDuplicateSignal = errors.New("reset signal already received")

// Signal source states.
const (
	SignalStateOK      = "ok"
	SignalStateError   = "error"
	SignalStateIdle    = "idle"
	SignalStateUnknown = "unknown"
)

var signalToken = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)
var signalID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidSignalProvider reports whether p is a usable provider name: lower-case
// letters, digits and . _ : - only. It must match the Provider of the accounts
// that the signal should affect (for example "codex" or "claude").
func ValidSignalProvider(p string) bool { return signalToken.MatchString(p) }

// NormalizeSignal validates a signal received from an untrusted source and
// trims its free text. Only reset_executed and reset_scheduled are accepted.
func NormalizeSignal(s ResetSignal) (ResetSignal, error) {
	s.Provider = strings.ToLower(strings.TrimSpace(s.Provider))
	s.Source = strings.ToLower(strings.TrimSpace(s.Source))
	s.ID = strings.TrimSpace(s.ID)
	if !ValidSignalProvider(s.Provider) {
		return s, errors.New("provider must be a lower-case identifier such as codex or claude")
	}
	if !signalToken.MatchString(s.Source) {
		return s, errors.New("source must be a lower-case identifier")
	}
	if !signalID.MatchString(s.ID) {
		return s, errors.New("id must be 1-128 characters of letters, digits and . _ : -")
	}
	if s.Kind == "" {
		s.Kind = SignalResetExecuted
	}
	if s.Kind != SignalResetExecuted && s.Kind != SignalResetScheduled {
		return s, errors.New("kind must be reset_executed or reset_scheduled")
	}
	if len(s.Text) > 500 {
		s.Text = s.Text[:500]
	}
	s.Text = strings.ToValidUTF8(strings.TrimSpace(s.Text), "")
	if s.URL != "" && !strings.HasPrefix(s.URL, "https://") {
		s.URL = ""
	}
	if len(s.URL) > 512 {
		s.URL = ""
	}
	return s, nil
}
