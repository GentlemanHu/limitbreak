// Package adapters contains closed, typed registries for supported integrations.
package adapters

import (
	"context"
	"errors"
	"sort"
	"time"

	"quota-session-orchestrator/internal/bridge"
	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/direct"
	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/executor"
	"quota-session-orchestrator/internal/proxy"
	"quota-session-orchestrator/internal/store"
)

type QuotaSource interface {
	Refresh(context.Context) ([]domain.Account, error)
	Capabilities() domain.Capabilities
}

type SessionObserver interface {
	Observe(context.Context, domain.HookEvent) error
}
type ResumeExecutor interface {
	Resume(context.Context, domain.Binding, string) (domain.BridgeResult, error)
	Probe(context.Context) domain.BridgeResult
}

type EventHandler func(context.Context, domain.HookEvent) error
type QuotaFactory func(context.Context, config.Config, config.Source, *store.Store) (QuotaSource, error)
type ObserverFactory func(EventHandler) (SessionObserver, error)
type ExecutorFactory func(string) (ResumeExecutor, error)

type Registry struct {
	quota     map[string]QuotaFactory
	observers map[string]ObserverFactory
	executors map[string]ExecutorFactory
}

// Registries are immutable after construction; there is no dynamic plugin loader.
func NewRegistry() *Registry {
	return &Registry{
		quota: map[string]QuotaFactory{"cliproxyapi": newCPA, "readonly-http": newHTTP},
		observers: map[string]ObserverFactory{"codehooks": func(h EventHandler) (SessionObserver, error) {
			if h == nil {
				return nil, errors.New("code hook event handler is required")
			}
			return codeHooks{h}, nil
		}},
		executors: map[string]ExecutorFactory{"ax": func(path string) (ResumeExecutor, error) {
			if path == "" {
				return nil, errors.New("explicit desktop bridge path is required")
			}
			return bridge.New(path), nil
		}},
	}
}

func (r *Registry) Quota(ctx context.Context, c config.Config, s config.Source, db *store.Store) (QuotaSource, error) {
	factory, ok := r.quota[s.Type]
	if !ok {
		return nil, errors.New("quota adapter is not implemented")
	}
	return factory(ctx, c, s, db)
}
func (r *Registry) Observer(id string, h EventHandler) (SessionObserver, error) {
	factory, ok := r.observers[id]
	if !ok {
		return nil, errors.New("session observer is not implemented")
	}
	return factory(h)
}
func (r *Registry) Executor(id, path string) (ResumeExecutor, error) {
	factory, ok := r.executors[id]
	if !ok {
		return nil, errors.New("resume executor is not implemented")
	}
	return factory(path)
}

// ConfiguredExecutor routes each binding to its host application. A configured
// App IPC is authoritative for desktop; failure never selects another identity
// or another channel.
func ConfiguredExecutor(c config.Config) (ResumeExecutor, error) {
	var desktop ResumeExecutor
	if c.DesktopSocket != "" || c.DesktopTokenFile != "" {
		if c.DesktopSocket == "" || c.DesktopTokenFile == "" {
			return nil, errors.New("desktop IPC socket and token file must be configured together")
		}
		desktop = bridge.NewAppIPC(c.DesktopSocket, c.DesktopTokenFile)
	} else {
		diagnostic, err := NewRegistry().Executor("ax", c.BridgePath)
		if err != nil {
			return nil, err
		}
		desktop = diagnostic
	}
	clients := map[string]executor.Backend{}
	for _, client := range c.EffectiveClients() {
		clients[client.ID] = executor.NewCommand(executor.ClientSpec{ID: client.ID, Command: client.Command, Env: client.Env,
			Timeout: time.Duration(client.TimeoutSeconds) * time.Second})
	}
	return executor.NewRouter(desktop, executor.NewTerminal()).WithDirect(direct.NewSender()).WithClients(clients), nil
}

type codeHooks struct{ handle EventHandler }

func (h codeHooks) Observe(ctx context.Context, event domain.HookEvent) error {
	// The authenticated Code hook ingress supplies the identity; no product probing.
	if event.Kind == "" {
		event.Kind = "code"
	}
	return h.handle(ctx, event)
}

func Descriptors() []domain.AdapterDescriptor {
	d := []domain.AdapterDescriptor{
		{ID: "cliproxyapi", Name: "CLIProxyAPI 额度来源", Kind: "quota_source", Implemented: true, ReadOnly: true,
			Capabilities: []string{"account_listing", "provider_quota", "quota_groups", "reset_anchors"},
			Limitations:  []string{"必须显式提供管理凭据引用", "仅读取 CPA 的账号与额度观测；不切换账号、不修改路由"}},
		{ID: "readonly-http", Name: "声明式 HTTP 额度来源", Kind: "quota_source", Implemented: true, ReadOnly: true,
			Capabilities: []string{"get_normalized_v1", "quota_groups"},
			Limitations:  []string{"必须明确配置 HTTPS URL，或使用字面回环 IP 的 HTTP URL", "禁止重定向；凭据可省略，配置时只能引用显式环境变量", "仅接受 normalized-v1 自定义协议；不加载任意解析器、脚本或动态代码"}},
		{ID: "codehooks", Name: "Code 会话生命周期 Hook", Kind: "session_observer", Implemented: true, ReadOnly: true,
			Capabilities: []string{"explicit_hook_ingress", "event_deduplication", "event_watermarks"},
			Limitations:  []string{"必须显式安装 Code 生命周期 Hook", "不提供独立 Claude、Codex 或 CCSwitch 客户端的会话发现"}},
		{ID: "app-ipc", Name: "原生 App 辅助功能执行器", Kind: "resume_executor", Implemented: true, ReadOnly: false,
			Capabilities: []string{"single_permission_identity", "authenticated_local_ipc", "deep_link_navigation", "locate", "explicit_target_resume", "submission_confirmation_required"},
			Limitations:  []string{"主 App 必须运行、已授权并解锁桌面", "Claude 桌面 Code 会话通过深链接定位，输入框必须为空", "未知布局、目标歧义或结果不确定均停止，不采用坐标兜底"}},
		{ID: "terminal-cli", Name: "终端 claude CLI 执行器", Kind: "resume_executor", Implemented: true, ReadOnly: false,
			Capabilities: []string{"tmux_send_keys", "headless_resume", "process_identity_verification", "locate", "submission_confirmation_required"},
			Limitations: []string{"仍开着的会话必须运行在 tmux 中；不在 tmux 中时只等待，关闭后改为后台 claude -p --resume 续写",
				"无法检查 claude 输入框里是否有未发送的草稿，只依赖键鼠空闲判定", "后台续写不经过终端，需要权限确认的工具调用会被拒绝",
				"后台续写期间不要在终端里重新打开同一会话", "结果不确定时停止，不重发"}},
		{ID: "ax", Name: "独立辅助功能诊断器", Kind: "resume_executor", Implemented: true, ReadOnly: true,
			Capabilities: []string{"preflight", "bounded_structure_inspection"},
			Limitations:  []string{"独立诊断具有自己的权限身份，不能代表主 App", "3.1 原生恢复通过 App IPC 执行；诊断程序不授权发送"}},
		{ID: "client-events", Name: "通用客户端事件入口", Kind: "session_observer", Implemented: true, ReadOnly: true,
			Capabilities: []string{"orchestrator_event_cli", "authenticated_hook_ingress", "event_deduplication", "provider_declaration"},
			Limitations: []string{"客户端须自行（或通过包装脚本）调用 orchestrator event 上报 limit_hit / turn_started / turn_finished 等事件",
				"只接受在 clients 中配置过的客户端 ID；不自动发现任何客户端的会话"}},
		{ID: "client-command", Name: "通用客户端命令执行器", Kind: "resume_executor", Implemented: true, ReadOnly: false,
			Capabilities: []string{"configured_argv_without_shell", "whole_argument_placeholders", "bounded_runtime", "submission_confirmation_required"},
			Limitations: []string{"命令由用户配置并对其行为负责；守护进程只按参数直接执行，不经过 shell",
				"命令启动后须由客户端上报 turn_started 或 turn_finished 确认，否则按超时转为需要处理，不重发",
				"Codex CLI、WorkBuddy、DeepSeek 等客户端的具体命令未经本项目实测"}},
		{ID: "codex-resets", Name: "Codex Resets 重置信号", Kind: "reset_signal", Implemented: true, ReadOnly: true,
			Capabilities: []string{"keyless_public_status", "etag_conditional_requests", "follow_up_rechecks"},
			Limitations:  []string{"信号只触发重新查询 Codex 额度，不作为额度已恢复的证据", "数据来自第三方站点 codex-resets.com；预测（watch）只展示不参与调度"}},
		{ID: "webhook-signals", Name: "Webhook 重置信号", Kind: "reset_signal", Implemented: true, ReadOnly: true,
			Capabilities: []string{"bearer_or_hmac_sha256", "replay_window", "deduplication"},
			Limitations:  []string{"仅监听本机回环地址；外部服务需自行转发", "信号只触发重新查询，不作为额度证据"}},
	}
	for _, product := range []struct{ id, name string }{{"claude-standalone", "Claude 独立客户端（未集成）"}, {"ccswitch", "CCSwitch（未集成）"}} {
		d = append(d, domain.AdapterDescriptor{ID: product.id, Name: product.name, Kind: "quota_source", Implemented: false, ReadOnly: true,
			Capabilities: []string{}, Limitations: []string{"尚未实现原生集成；不得根据显示名称推断账号或重新绑定会话"}})
	}
	sort.Slice(d, func(i, j int) bool { return d[i].ID < d[j].ID })
	return d
}

func newCPA(ctx context.Context, c config.Config, s config.Source, db *store.Store) (QuotaSource, error) {
	if s.CredentialEnv == "" {
		return nil, errors.New("CPA requires an explicit credential_env reference")
	}
	if err := config.ValidateCredentialEnv(s.CredentialEnv); err != nil {
		return nil, err
	}
	c.Sources = nil
	c.ProxyURL = s.URL
	c.ManagementKeyEnv = s.CredentialEnv
	c.ManagementKeyFile = "" // Legacy file handling belongs solely to main.loadManagementKey.
	c.ManagementPrefix = s.ManagementPrefix
	if c.ManagementPrefix == "" {
		c.ManagementPrefix = "auto"
	}
	c.OnlyEmail = s.OnlyEmail
	if s.PreferPlus != nil {
		c.PreferPlus = *s.PreferPlus
	}
	if s.PollSeconds != 0 {
		c.PollSeconds = s.PollSeconds
	}
	if s.HTTPTimeoutSeconds != 0 {
		c.HTTPTimeoutSeconds = s.HTTPTimeoutSeconds
	}
	c.Queries = s.Queries
	q := proxy.New(c)
	if db != nil {
		if err := q.SetAnchorStorage(ctx, db.QuotaAnchorNamespace(s.ID)); err != nil {
			return nil, err
		}
	}
	return q, nil
}
