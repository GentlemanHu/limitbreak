package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Host applications a Code session can be observed in. The value is derived
// solely from the hook process environment; never from window titles.
const (
	AppClaudeDesktop = "claude-desktop"
	AppTerminal      = "terminal"
	AppUnknown       = "unknown"
	// AppClient is a session reported by a configured generic client through
	// `orchestrator event --client <id>` (Codex CLI, other agents, scripts).
	AppClient = "client"
)

// ClientExecutorPrefix + client id is the executor of a generic client session.
const ClientExecutorPrefix = "client:"

var (
	clientIDPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	clientSessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// ValidClientID accepts a configured client identifier such as "codex-cli".
func ValidClientID(id string) bool { return clientIDPattern.MatchString(id) }

// ValidClientSessionID accepts session IDs that are safe to pass as a single
// command argument: they cannot start with "-" and contain no spaces or paths.
func ValidClientSessionID(id string) bool { return clientSessionPattern.MatchString(id) }

// ClientExecutor names the executor of client id.
func ClientExecutor(id string) string { return ClientExecutorPrefix + id }

// Session host modes. Follow ("") inherits Policy.DefaultHostMode.
const (
	HostFollow  = ""
	HostHosted  = "hosted"
	HostMonitor = "monitor"
	HostOff     = "off"
)

// Delivery channels. The desktop App types into its own Code composer; the
// terminal executor types into the tmux pane of a live claude CLI, or resumes
// a closed CLI session headlessly with `claude -p --resume`.
const (
	ExecutorDesktop  = "claude-desktop"
	ExecutorTerminal = "claude-terminal"
)

// ChannelDirect marks results delivered through a live session's own
// cross-session messaging socket instead of its UI or terminal.
const ChannelDirect = "direct"

// Direct delivery policy values; "" behaves as DirectAuto.
const (
	DirectAuto = "auto"
	DirectOff  = "off"
)

func ValidDirectDelivery(v string) bool { return v == "" || v == DirectAuto || v == DirectOff }

// ValidSwitch accepts the shared ""/"auto"/"off" policy switch values used by
// restore_focus and claude_quiet; "" behaves as "auto".
func ValidSwitch(v string) bool { return v == "" || v == DirectAuto || v == DirectOff }

const DesktopBundleID = "com.anthropic.claudefordesktop"

var (
	hostSessionPattern = regexp.MustCompile(`^local_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	cliSessionPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	tmuxPanePattern    = regexp.MustCompile(`^%[0-9]{1,9}$`)
)

// SessionOrigin is what the hook process can prove about where the session runs.
type SessionOrigin struct {
	App           string `json:"app"`
	Entrypoint    string `json:"entrypoint,omitempty"`
	TermProgram   string `json:"term_program,omitempty"`
	BundleID      string `json:"bundle_id,omitempty"`
	HostSessionID string `json:"host_session_id,omitempty"`
	ClaudePID     int    `json:"claude_pid,omitempty"`
	Ancestors     []int  `json:"ancestors,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`
	// ExecPath is the claude executable that ran the session; it is used only to
	// resume a closed terminal session headlessly.
	ExecPath string `json:"exec_path,omitempty"`
	// Tmux facts come from TMUX ("socket,server_pid,session") and TMUX_PANE.
	TmuxSocket    string `json:"tmux_socket,omitempty"`
	TmuxServerPID int    `json:"tmux_server_pid,omitempty"`
	TmuxPane      string `json:"tmux_pane,omitempty"`
	// Detached marks that ClaudePID no longer hosts this session: it ended, or
	// the same process reported a different session since.
	Detached bool `json:"detached,omitempty"`
	// Client is the configured generic client that reported the session; it is
	// never set by the Claude Code hook.
	Client     string    `json:"client,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// ClassifyOrigin derives App from environment facts only.
func ClassifyOrigin(o SessionOrigin) string {
	switch {
	case o.Client != "":
		return AppClient
	case o.BundleID == DesktopBundleID || strings.HasPrefix(o.Entrypoint, "claude-desktop"):
		return AppClaudeDesktop
	case o.TermProgram != "" || o.BundleID != "" || o.TmuxPane != "" || o.Entrypoint == "cli" || o.Entrypoint == "sdk-cli":
		return AppTerminal
	}
	return AppUnknown
}

func ValidHostSessionID(id string) bool { return hostSessionPattern.MatchString(id) }

// ValidCLISessionID accepts the UUID session IDs claude --resume takes.
func ValidCLISessionID(id string) bool { return cliSessionPattern.MatchString(id) }

func ValidTmuxPane(p string) bool { return tmuxPanePattern.MatchString(p) }

// ValidAbsolutePath accepts a clean, single-line absolute path.
func ValidAbsolutePath(p string) bool {
	return strings.HasPrefix(p, "/") && plainField(p, 1024) && !strings.Contains(p, "/../") && !strings.HasSuffix(p, "/..")
}

// DesktopDeepLink opens exactly one Claude desktop Code session.
func DesktopDeepLink(hostSessionID string) string {
	if !ValidHostSessionID(hostSessionID) {
		return ""
	}
	return "claude://claude.ai/epitaxy/" + hostSessionID
}

// ValidDesktopDeepLink accepts exactly the links DesktopDeepLink produces.
func ValidDesktopDeepLink(link string) bool {
	id, ok := strings.CutPrefix(link, "claude://claude.ai/epitaxy/")
	return ok && ValidHostSessionID(id)
}

func plainField(s string, max int) bool {
	return utf8.ValidString(s) && len(s) <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}

func ValidateOrigin(o SessionOrigin) error {
	switch o.App {
	case AppClaudeDesktop, AppTerminal, AppUnknown:
		if o.Client != "" {
			return errors.New("origin client is only valid for client sessions")
		}
	case AppClient:
		if !ValidClientID(o.Client) {
			return errors.New("origin client is invalid")
		}
	default:
		return errors.New("origin app is invalid")
	}
	for _, f := range []string{o.Entrypoint, o.TermProgram, o.BundleID, o.EngineVersion} {
		if !plainField(f, 128) {
			return errors.New("origin field is invalid")
		}
	}
	if o.HostSessionID != "" && !ValidHostSessionID(o.HostSessionID) {
		return errors.New("origin host session is invalid")
	}
	if o.ClaudePID < 0 || len(o.Ancestors) > 16 {
		return errors.New("origin process chain is invalid")
	}
	for _, pid := range o.Ancestors {
		if pid <= 1 {
			return errors.New("origin process chain is invalid")
		}
	}
	if (o.ExecPath != "" && !ValidAbsolutePath(o.ExecPath)) || (o.TmuxSocket != "" && !ValidAbsolutePath(o.TmuxSocket)) {
		return errors.New("origin path is invalid")
	}
	if o.TmuxPane != "" && !ValidTmuxPane(o.TmuxPane) {
		return errors.New("origin tmux pane is invalid")
	}
	// The three tmux facts are only meaningful together.
	if (o.TmuxPane == "") != (o.TmuxSocket == "") || (o.TmuxPane == "") != (o.TmuxServerPID == 0) || o.TmuxServerPID < 0 || o.TmuxServerPID == 1 {
		return errors.New("origin tmux facts are incomplete")
	}
	return nil
}

// NormalizeModel strips context-window suffixes such as "[1m]" and casing.
func NormalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.IndexByte(m, '['); i > 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}

// ModelIn compares normalized names; an empty wanted model matches any account.
func ModelIn(models []string, wanted string) bool {
	if wanted == "" {
		return true
	}
	w := NormalizeModel(wanted)
	for _, m := range models {
		if NormalizeModel(m) == w {
			return true
		}
	}
	return false
}

// ProviderFamily is a last-resort inference used only when no model matched.
func ProviderFamily(model string) string {
	m := NormalizeModel(model)
	switch {
	case m == "", strings.HasPrefix(m, "claude"), strings.Contains(m, "opus"), strings.Contains(m, "sonnet"), strings.Contains(m, "haiku"):
		return "claude"
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "codex"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "codex"
	case strings.HasPrefix(m, "gemini"):
		return "gemini"
	}
	return ""
}

func EffectiveHostMode(s Session, p Policy) string {
	if s.HostMode != HostFollow {
		return s.HostMode
	}
	if p.DefaultHostMode != HostFollow {
		return p.DefaultHostMode
	}
	return HostHosted
}

func ValidHostMode(mode string, allowFollow bool) bool {
	switch mode {
	case HostHosted, HostMonitor, HostOff:
		return true
	case HostFollow:
		return allowFollow
	}
	return false
}

// PoolView explains which accounts a session is matched to, and why.
type PoolView struct {
	Model      string   `json:"model"`
	Provider   string   `json:"provider,omitempty"`
	AccountIDs []string `json:"account_ids"`
	Basis      string   `json:"basis"`
	Inferred   bool     `json:"inferred"`
}

// HostView explains the effective hosting decision for one session.
type HostView struct {
	Mode     string `json:"mode"`
	Executor string `json:"executor,omitempty"`
	Target   string `json:"target,omitempty"`
	Ready    bool   `json:"ready"`
	Problem  string `json:"problem,omitempty"`
}
