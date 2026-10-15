package domain

import "time"

const (
	Running     = "RUNNING"
	Completed   = "COMPLETED"
	Cooldown    = "COOLDOWN"
	Ready       = "READY_TO_RESUME"
	Dispatching = "DISPATCHING"
	Verifying   = "VERIFYING"
	Attention   = "NEEDS_ATTENTION"
	Cancelled   = "CANCELLED"
)

type Window struct {
	ID           string     `json:"id,omitempty"`
	Description  string     `json:"description,omitempty"`
	Name         string     `json:"name"`
	UsedPercent  *float64   `json:"used_percent,omitempty"`
	Blocked      bool       `json:"blocked"`
	ResetAt      *time.Time `json:"reset_at,omitempty"`
	LimitSeconds int64      `json:"limit_seconds,omitempty"`
}

type Account struct {
	SourceID    string        `json:"source_id"`
	ID          string        `json:"id"`
	AuthIndex   string        `json:"auth_index"`
	Provider    string        `json:"provider"`
	Email       string        `json:"email"`
	Plan        string        `json:"plan"`
	Models      []string      `json:"models"`
	State       string        `json:"state"`
	Reason      string        `json:"reason"`
	Windows     []Window      `json:"windows"`
	CheckedAt   time.Time     `json:"checked_at"`
	NextCheckAt time.Time     `json:"next_check_at"`
	AvailableAt *time.Time    `json:"available_at,omitempty"`
	Disabled    bool          `json:"disabled"`
	Groups      []QuotaGroup  `json:"groups"`
	Metrics     []QuotaMetric `json:"metrics"`
	QuotaSource string        `json:"quota_source"`
	QuotaError  string        `json:"quota_error,omitempty"`
}

// Bindings are explicitly enrolled by the user; account routing is not inferred.
type Binding struct {
	SessionID                            string        `json:"session_id"`
	Kind                                 string        `json:"kind"`
	Provider                             string        `json:"provider"`
	Model                                string        `json:"model"`
	AccountIDs                           []string      `json:"account_ids"`
	TargetTitle                          string        `json:"target_title"`
	TargetIdentifier                     string        `json:"target_identifier"`
	ProjectMarker                        string        `json:"project_marker"`
	DeepLink                             string        `json:"deep_link"`
	AutoResume                           bool          `json:"auto_resume"`
	Priority                             int           `json:"priority"`
	AcknowledgeSideEffects               bool          `json:"acknowledge_side_effects"`
	MonitorResources                     []ResourceRef `json:"monitor_resources,omitempty"`
	MonitorMode                          string        `json:"monitor_mode,omitempty"`
	ResumeResources                      []ResourceRef `json:"resume_resources,omitempty"`
	ContinueMessage                      string        `json:"continue_message,omitempty"`
	ResumeOwner                          string        `json:"resume_owner,omitempty"`
	NativeAutoResumeDisabledAcknowledged bool          `json:"native_auto_resume_disabled_acknowledged,omitempty"`
	TargetWindowIdentifier               string        `json:"target_window_identifier,omitempty"`
	TargetPaneIdentifier                 string        `json:"target_pane_identifier,omitempty"`
	// Executor/Origin/TargetOverride/Inferred are set on engine-derived plans.
	Executor                 string         `json:"executor,omitempty"`
	Origin                   *SessionOrigin `json:"origin,omitempty"`
	TargetOverride           string         `json:"target_override,omitempty"`
	Inferred                 bool           `json:"inferred,omitempty"`
	WorkDir                  string         `json:"work_dir,omitempty"` // terminal sessions: the directory claude --resume runs in
	ExecutionGeneration      int64          `json:"-"`
	ExecutionLease           string         `json:"-"`
	ExecutionUserIdleSeconds int            `json:"-"`
	// ExecutionDirect lets the executor deliver through the session's own
	// messaging socket when the session process is live.
	ExecutionDirect bool `json:"-"`
	// ExecutionRestoreFocus puts the user's previous app back in front after
	// Claude was opened; ExecutionQuietSeconds (0 = off) waits until Claude
	// itself has sat idle that long before its visible session is switched.
	ExecutionRestoreFocus bool `json:"-"`
	ExecutionQuietSeconds int  `json:"-"`
}

type Session struct {
	ID         string         `json:"id"`
	Cwd        string         `json:"cwd"`
	Title      string         `json:"title"`
	Kind       string         `json:"kind"`
	Model      string         `json:"model"`
	Provider   string         `json:"provider"`
	State      string         `json:"state"`
	Error      string         `json:"error"`
	Reason     string         `json:"reason"`
	Generation int64          `json:"generation"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Binding    *Binding       `json:"binding,omitempty"`
	Monitor    *MonitorStatus `json:"monitor,omitempty"`
	// Per-session hosting settings (persisted).
	Origin          *SessionOrigin `json:"origin,omitempty"`
	HostMode        string         `json:"host_mode,omitempty"`
	ContinueMessage string         `json:"continue_message,omitempty"`
	PoolOverride    []string       `json:"pool_override,omitempty"`
	TargetOverride  string         `json:"target_override,omitempty"`
	NativeResume    string         `json:"native_resume,omitempty"`
	Priority        int            `json:"priority,omitempty"`
	// Facts from the Claude desktop session list (metadata only).
	InCatalog      bool       `json:"in_catalog,omitempty"`
	Archived       bool       `json:"archived,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	// Computed in snapshots only.
	Pool       *PoolView `json:"pool,omitempty"`
	Host       *HostView `json:"host,omitempty"`
	ReasonText string    `json:"reason_text,omitempty"`
}

type Job struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"session_id"`
	Generation int64      `json:"generation"`
	State      string     `json:"state"`
	AccountID  string     `json:"account_id"`
	NotBefore  time.Time  `json:"not_before"`
	LeaseUntil *time.Time `json:"lease_until,omitempty"`
	Attempts   int        `json:"attempts"`
	Reason     string     `json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	// Kind is "" (resume) or "test" (explicit user test send, no quota gate).
	Kind          string     `json:"kind,omitempty"`
	ResetHint     *time.Time `json:"reset_hint,omitempty"`
	Message       string     `json:"message,omitempty"`
	MessageDigest string     `json:"message_digest,omitempty"`
	ReasonText    string     `json:"reason_text,omitempty"`
	// Delivery records how a submitted continue reached the session: one of
	// the Delivery* values, empty until a send was handed to an executor.
	Delivery string `json:"delivery,omitempty"`
}

// How a continue reached its session, for display.
const (
	DeliveryDirect         = "direct"            // the session's own message socket, process already running
	DeliveryWokenDirect    = "woken_direct"      // session opened in the background, then the message socket
	DeliveryWindowFallback = "window_after_wake" // the session process never started; typed into the window
	DeliveryWindow         = "window"            // typed into the Claude window or terminal
)

type Audit struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	SessionID string    `json:"session_id"`
	Message   string    `json:"message"`
	// Text is the user-facing explanation of Message; computed, never stored.
	Text string `json:"text,omitempty"`
}

type HookEvent struct {
	EventID          string         `json:"event_id"`
	SessionID        string         `json:"session_id"`
	Cwd              string         `json:"cwd"`
	HookEventName    string         `json:"hook_event_name"`
	Error            string         `json:"error"`
	ErrorDetails     string         `json:"error_details"`
	Model            string         `json:"model"`
	NewModel         string         `json:"new_model"`
	SessionTitle     string         `json:"session_title"`
	Source           string         `json:"source"`
	Kind             string         `json:"kind,omitempty"`
	Reason           string         `json:"reason"`
	ObservedAt       time.Time      `json:"observed_at"`
	NotificationType string         `json:"notification_type,omitempty"`
	Origin           *SessionOrigin `json:"origin,omitempty"`
	// Provider is declared by a configured generic client (for example
	// "codex"); it selects the account pool when the model alone cannot.
	Provider string `json:"provider,omitempty"`
	// Never store the user's prompt or full transcript.
}

type Policy struct {
	Paused                 bool   `json:"paused"`
	MaxRunning             int    `json:"max_running"`
	MaxPerAccount          int    `json:"max_per_account"`
	SafetySeconds          int    `json:"safety_seconds"`
	FreshnessSeconds       int    `json:"freshness_seconds"`
	VerifySeconds          int    `json:"verify_seconds"`
	LongProbeSeconds       int    `json:"long_probe_seconds"`
	UnknownProbeSeconds    int    `json:"unknown_probe_seconds"`
	MinSendIntervalSeconds int    `json:"min_send_interval_seconds"`
	ContinueMessage        string `json:"continue_message"`
	EarlyProbeSeconds      int    `json:"early_probe_seconds,omitempty"`
	NearResetProbeSeconds  int    `json:"near_reset_probe_seconds,omitempty"`
	ResetLookaheadSeconds  int    `json:"reset_lookahead_seconds,omitempty"`
	UserIdleSeconds        int    `json:"user_idle_seconds,omitempty"`
	RunningStaleSeconds    int    `json:"running_stale_seconds,omitempty"`
	QueueAgingSeconds      int    `json:"queue_aging_seconds,omitempty"`
	MaxAutoResumesPerHour  int    `json:"max_auto_resumes_per_hour,omitempty"`
	// Hosting. Hosted sessions never send before AutomationConsentAt is set.
	DefaultHostMode     string            `json:"default_host_mode,omitempty"`
	ModelAliases        map[string]string `json:"model_aliases,omitempty"`
	AutomationConsentAt *time.Time        `json:"automation_consent_at,omitempty"`
	NativeGraceSeconds  int               `json:"native_grace_seconds,omitempty"`
	// DirectDelivery is "" or "auto" (prefer the silent background channel) or
	// "off" (always use the UI channel).
	DirectDelivery string `json:"direct_delivery,omitempty"`
	// ResetFallback is "" or "auto" (when the quota query itself is throttled,
	// try once after the recorded reset has passed) or "off" (wait for a
	// confirmed quota reading).
	ResetFallback string `json:"reset_fallback,omitempty"`
	// RestoreFocus is "" or "auto" (after Claude was opened, put the previously
	// frontmost app back) or "off".
	RestoreFocus string `json:"restore_focus,omitempty"`
	// ClaudeQuiet is "" or "auto" (only switch Claude's visible session after
	// Claude has sat idle ClaudeQuietSeconds, default 120) or "off".
	ClaudeQuiet        string `json:"claude_quiet,omitempty"`
	ClaudeQuietSeconds int    `json:"claude_quiet_seconds,omitempty"`
}

type Capabilities struct {
	Sources              []SourceHealth `json:"sources"`
	ProxyVersion         string         `json:"proxy_version"`
	ManagementPrefix     string         `json:"management_prefix"`
	ProxyConnected       bool           `json:"proxy_connected"`
	BridgeAvailable      bool           `json:"bridge_available"`
	BridgeChecked        bool           `json:"bridge_checked"`
	AccessibilityGranted bool           `json:"accessibility_granted"`
	ScreenLocked         bool           `json:"screen_locked"`
	DesktopCheckedAt     *time.Time     `json:"desktop_checked_at,omitempty"`
	DesktopCode          string         `json:"desktop_code,omitempty"`
	AccessibilityState   string         `json:"accessibility_state,omitempty"`
	Message              string         `json:"message"`
}

type Snapshot struct {
	Adapters     []AdapterDescriptor `json:"adapters"`
	Sources      []SourceHealth      `json:"sources"`
	Time         time.Time           `json:"time"`
	Accounts     []Account           `json:"accounts"`
	Sessions     []Session           `json:"sessions"`
	Jobs         []Job               `json:"jobs"`
	Audit        []Audit             `json:"audit"`
	Policy       Policy              `json:"policy"`
	PolicyDigest string              `json:"policy_digest"`
	Capabilities Capabilities        `json:"capabilities"`
	Signals      []SignalSourceState `json:"signals"`
}

type BridgeResult struct {
	OK                   bool   `json:"ok"`
	Submitted            bool   `json:"submitted"`
	Code                 string `json:"code"`
	Message              string `json:"message"`
	AccessibilityGranted bool   `json:"accessibility_granted"`
	ScreenLocked         bool   `json:"screen_locked"`
	// Channel names the transport that produced the result; "" is the
	// executor's own (UI or terminal) channel.
	Channel string `json:"channel,omitempty"`
	// Woken marks a result produced after the executor opened the session in
	// the background to start its process.
	Woken bool `json:"woken,omitempty"`
}
