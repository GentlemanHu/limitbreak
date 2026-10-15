package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/localapi"
	"quota-session-orchestrator/internal/store"
)

// All control tools create a pending request only. The daemon binds requestID to
// the action digest; reusing that ID with different parameters is a conflict.
type RefreshInput struct {
	RequestID  string `json:"requestID" jsonschema:"Caller-issued stable idempotency ID; reuse exactly the same action on retry"`
	TTLSeconds int    `json:"ttlSeconds" jsonschema:"Confirmation lifetime from 1 to 300 seconds"`
}

type PolicyInput struct {
	RequestID    string `json:"requestID"`
	TTLSeconds   int    `json:"ttlSeconds"`
	PolicyDigest string `json:"policyDigest" jsonschema:"Current 64-character policy digest from status; compare-and-swap required"`
}

type SessionInput struct {
	RequestID          string `json:"requestID"`
	TTLSeconds         int    `json:"ttlSeconds"`
	SessionID          string `json:"sessionID" jsonschema:"Exact opaque s_ handle from sessions; not a native session ID"`
	ExpectedGeneration int64  `json:"expectedGeneration" jsonschema:"Observed session generation; required compare-and-swap"`
}

type MonitorInput struct {
	RequestID          string               `json:"requestID"`
	TTLSeconds         int                  `json:"ttlSeconds"`
	SessionID          string               `json:"sessionID"`
	ExpectedGeneration int64                `json:"expectedGeneration"`
	MonitorMode        string               `json:"monitorMode" jsonschema:"any or all"`
	MonitorResources   []domain.ResourceRef `json:"monitorResources" jsonschema:"1 to 64 exact opaque resource references from resources; no resume resources"`
}

type AutoResumeInput struct {
	RequestID          string `json:"requestID"`
	TTLSeconds         int    `json:"ttlSeconds"`
	SessionID          string `json:"sessionID"`
	ExpectedGeneration int64  `json:"expectedGeneration"`
	Enabled            bool   `json:"enabled" jsonschema:"Request enabling or disabling automatic resumption; user must separately confirm"`
}

type SettingsInput struct {
	RequestID     string         `json:"requestID"`
	TTLSeconds    int            `json:"ttlSeconds"`
	PolicyDigest  string         `json:"policyDigest"`
	SettingsPatch map[string]int `json:"settingsPatch" jsonschema:"Nonempty allowlisted integer policy patch; no prompts, paused flag, credentials, hooks or URLs"`
}

type SessionSettingsInput struct {
	RequestID          string    `json:"requestID"`
	TTLSeconds         int       `json:"ttlSeconds"`
	SessionID          string    `json:"sessionID"`
	ExpectedGeneration int64     `json:"expectedGeneration"`
	HostMode           *string   `json:"hostMode,omitempty" jsonschema:"hosted, monitor, off, or empty to follow the global default; omit to keep"`
	ContinueMessage    *string   `json:"continueMessage,omitempty" jsonschema:"Single-line continue text up to 4096 bytes; empty follows the global text; omit to keep"`
	PoolOverride       *[]string `json:"poolOverride,omitempty" jsonschema:"Exact account IDs to route this session through; empty list follows the model mapping; omit to keep"`
	TargetOverride     *string   `json:"targetOverride,omitempty" jsonschema:"Empty for automatic or desktop; omit to keep"`
	Priority           *int      `json:"priority,omitempty" jsonschema:"Queue priority from -100 to 100; omit to keep"`
}

type TestSendInput struct {
	RequestID          string `json:"requestID"`
	TTLSeconds         int    `json:"ttlSeconds"`
	SessionID          string `json:"sessionID"`
	ExpectedGeneration int64  `json:"expectedGeneration"`
	Message            string `json:"message,omitempty" jsonschema:"Optional single-line text; empty uses the session's continue text"`
}

func registerControls(s *mcp.Server, b Backend) {
	mcp.AddTool(s, tool("request_refresh", "Request quota refresh (network side effect), NOT a read-only operation. Only creates a pending user confirmation.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in RefreshInput) (*mcp.CallToolResult, RequestOutput, error) {
			return request(ctx, b, in.RequestID, in.TTLSeconds, store.ControlAction{Operation: "refresh", SessionID: "daemon"})
		})
	for _, operation := range []string{"pause", "unpause"} {
		mcp.AddTool(s, tool("request_"+operation, "Request "+operation+" with policy CAS; only creates a pending user confirmation.", false),
			func(ctx context.Context, _ *mcp.CallToolRequest, in PolicyInput) (*mcp.CallToolResult, RequestOutput, error) {
				return request(ctx, b, in.RequestID, in.TTLSeconds, store.ControlAction{Operation: operation, SessionID: "daemon", PolicyDigest: in.PolicyDigest})
			})
	}
	mcp.AddTool(s, tool("request_cancel", "Request cancellation at the observed generation; user confirmation required. Does not promise retraction of an already-submitted send.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in SessionInput) (*mcp.CallToolResult, RequestOutput, error) {
			return request(ctx, b, in.RequestID, in.TTLSeconds, store.ControlAction{Operation: "cancel", SessionID: in.SessionID, ExpectedGeneration: in.ExpectedGeneration})
		})
	mcp.AddTool(s, tool("request_bind_monitor", "Request monitor-only binding to exact scoped inventory handles; never changes routing or resume resources. User confirmation required.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in MonitorInput) (*mcp.CallToolResult, RequestOutput, error) {
			a := store.ControlAction{Operation: "bind_monitor", SessionID: in.SessionID, ExpectedGeneration: in.ExpectedGeneration,
				Params: store.ControlParams{MonitorMode: in.MonitorMode, MonitorResources: in.MonitorResources}}
			return request(ctx, b, in.RequestID, in.TTLSeconds, a)
		})
	mcp.AddTool(s, tool("request_enable_auto_resume", "Request automatic-resume setting at the observed generation; only pending confirmation, never direct resume or self-approval.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in AutoResumeInput) (*mcp.CallToolResult, RequestOutput, error) {
			a := store.ControlAction{Operation: "enable_auto_resume", SessionID: in.SessionID, ExpectedGeneration: in.ExpectedGeneration, Params: store.ControlParams{Enabled: &in.Enabled}}
			return request(ctx, b, in.RequestID, in.TTLSeconds, a)
		})
	mcp.AddTool(s, tool("request_session_settings", "Request per-session hosting settings (mode, continue text, account pool, target, priority) at the observed generation; user confirmation required.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in SessionSettingsInput) (*mcp.CallToolResult, RequestOutput, error) {
			settings := domain.SessionSettings{HostMode: in.HostMode, ContinueMessage: in.ContinueMessage, PoolOverride: in.PoolOverride, TargetOverride: in.TargetOverride, Priority: in.Priority}
			a := store.ControlAction{Operation: "session_settings", SessionID: in.SessionID, ExpectedGeneration: in.ExpectedGeneration, Params: store.ControlParams{SessionSettings: &settings}}
			return request(ctx, b, in.RequestID, in.TTLSeconds, a)
		})
	mcp.AddTool(s, tool("request_test_send", "Request one real test delivery of continue text into the session's own window or terminal tab (outward side effect). Only creates a pending user confirmation; never retried when uncertain.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in TestSendInput) (*mcp.CallToolResult, RequestOutput, error) {
			a := store.ControlAction{Operation: "test_send", SessionID: in.SessionID, ExpectedGeneration: in.ExpectedGeneration, Params: store.ControlParams{Message: in.Message}}
			return request(ctx, b, in.RequestID, in.TTLSeconds, a)
		})
	mcp.AddTool(s, tool("request_locate", "Request bringing the session's own Claude window to the front without typing; user confirmation required.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in SessionInput) (*mcp.CallToolResult, RequestOutput, error) {
			return request(ctx, b, in.RequestID, in.TTLSeconds, store.ControlAction{Operation: "locate", SessionID: in.SessionID, ExpectedGeneration: in.ExpectedGeneration})
		})
	mcp.AddTool(s, tool("request_settings_patch", "Request allowlisted numeric settings patch using actual settingsPatch operation and policy CAS; user confirmation required.", false),
		func(ctx context.Context, _ *mcp.CallToolRequest, in SettingsInput) (*mcp.CallToolResult, RequestOutput, error) {
			patch := make(map[string]json.RawMessage, len(in.SettingsPatch))
			for key, value := range in.SettingsPatch {
				data, err := json.Marshal(value)
				if err != nil {
					return nil, RequestOutput{}, ErrRequest
				}
				patch[key] = data
			}
			a := store.ControlAction{Operation: "settingsPatch", SessionID: "daemon", PolicyDigest: in.PolicyDigest, Params: store.ControlParams{SettingsPatch: patch}}
			return request(ctx, b, in.RequestID, in.TTLSeconds, a)
		})
}

func request(ctx context.Context, b Backend, id string, ttl int, action store.ControlAction) (*mcp.CallToolResult, RequestOutput, error) {
	in := localapi.ActionInput{RequestID: id, TTLSeconds: ttl, Action: action}
	if localapi.ValidateActionInput(in) != nil {
		return nil, RequestOutput{}, ErrRequest
	}
	out, err := b.RequestAction(ctx, in)
	if err != nil || localapi.ValidateRequest(out.Request) != nil || out.Request.ID != id || store.ControlDigest(out.Request.Action) != store.ControlDigest(action) {
		return nil, RequestOutput{}, ErrRequest
	}
	// An idempotent lookup may return an already confirmed or expired record; it
	// never approves it again, extends its TTL, executes it or retries a send.
	return nil, RequestOutput{Request: summary(out.Request), RequiresConfirmation: true}, nil
}
