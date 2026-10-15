package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"quota-session-orchestrator/internal/domain"
)

// Hosting reasons are user-facing Chinese and pass through domain.Explain.
const (
	reasonMonitorOnly    = "仅监控：不会自动发送"
	reasonHostOff        = "已关闭托管"
	reasonNoConsent      = "尚未在首次设置中授权自动继续"
	reasonNotCode        = "仅 Code 会话可自动继续"
	reasonNoPool         = "没有与该会话模型匹配的账号"
	reasonNoOrigin       = "未采集到会话来源（请安装 Hook 后在会话里发一条消息）"
	reasonNoDesktopID    = "桌面会话缺少可定位的会话 ID"
	reasonUnsupportedApp = "无法识别会话所在的应用，仅监控"
	reasonNoProcess      = "未采集到终端 claude 进程，无法判断会话是否仍开着（请升级 Hook 后在会话里发一条消息）"
	reasonNoTerminalPath = "终端会话既不在 tmux 中，也没有可用于后台续写的 claude 路径与工作目录，仅监控"
	reasonBadTarget      = "发送目标设置已失效，请在会话设置中重新选择"
	reasonBadMessage     = "继续文案无效（须为单行、非空、≤4096 字节）"
	reasonLegacyNative   = "旧版绑定指定由原生/手动继续，仅监控"
	reasonNativeGrace    = "等待原生自动继续（宽限期）"
	reasonResetFallback  = "额度查询暂不可用，记录的重置时间已过，尝试继续"
)

func digestText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// legacyGuard reports whether a session keeps its explicit legacy guard binding.
func legacyGuard(s domain.Session) bool {
	return s.Binding != nil && s.Binding.ResumeOwner == "guard"
}

func sessionPriority(s domain.Session) int {
	if legacyGuard(s) {
		return s.Binding.Priority
	}
	return s.Priority
}

// resolvePool matches accounts to the session model: manual override, alias,
// exact normalized model, model prefix and finally provider family.
func resolvePool(accounts []domain.Account, s domain.Session, p domain.Policy) domain.PoolView {
	view := domain.PoolView{AccountIDs: []string{}}
	usable := func(a domain.Account) bool { return !a.Disabled && a.ID != "" }
	collect := func(match func(domain.Account) bool) []domain.Account {
		var out []domain.Account
		for _, a := range accounts {
			if usable(a) && match(a) {
				out = append(out, a)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	finish := func(list []domain.Account, model, basis string, inferred bool) domain.PoolView {
		view.Model, view.Basis, view.Inferred = model, basis, inferred
		providers := map[string]bool{}
		for _, a := range list {
			view.AccountIDs = append(view.AccountIDs, a.ID)
			providers[a.Provider] = true
		}
		if len(providers) == 1 {
			for k := range providers {
				view.Provider = k
			}
		}
		return view
	}
	if len(s.PoolOverride) > 0 {
		list := collect(func(a domain.Account) bool { return contains(s.PoolOverride, a.ID) })
		return finish(list, "", "manual", false)
	}
	norm := domain.NormalizeModel(s.Model)
	wanted, basis := norm, "exact"
	if alias, ok := p.ModelAliases[norm]; ok && norm != "" {
		wanted, basis = domain.NormalizeModel(alias), "alias"
	}
	if wanted != "" {
		if list := collect(func(a domain.Account) bool { return domain.ModelIn(a.Models, wanted) }); len(list) > 0 {
			return finish(list, wanted, basis, false)
		}
		matched := map[string]bool{}
		list := collect(func(a domain.Account) bool {
			for _, m := range a.Models {
				n := domain.NormalizeModel(m)
				if n != "" && (strings.HasPrefix(wanted, n) || strings.HasPrefix(n, wanted)) {
					matched[n] = true
					return true
				}
			}
			return false
		})
		if len(list) > 0 {
			model := ""
			if len(matched) == 1 {
				for k := range matched {
					model = k
				}
			}
			return finish(list, model, "prefix", true)
		}
	}
	// A generic client declares its provider; only Code hooks infer it from the model.
	family := s.Provider
	if family == "" {
		family = domain.ProviderFamily(s.Model)
	}
	if family != "" {
		list := collect(func(a domain.Account) bool { return strings.EqualFold(a.Provider, family) })
		view := finish(list, "", "provider", true)
		if len(list) > 0 {
			view.Provider = family
		}
		return view
	}
	view.Basis = "none"
	return view
}

// resolveTarget locates the delivery executor from the observed session origin.
func resolveTarget(s domain.Session) (domain.Binding, string) {
	b := domain.Binding{SessionID: s.ID, Kind: "code", Origin: s.Origin, TargetOverride: s.TargetOverride}
	switch {
	case !domain.ValidTargetOverride(s.TargetOverride):
		return b, reasonBadTarget
	case s.TargetOverride == "desktop":
		if s.Origin == nil || domain.DesktopDeepLink(s.Origin.HostSessionID) == "" {
			return b, reasonNoDesktopID
		}
		b.Executor = domain.ExecutorDesktop
		b.DeepLink = domain.DesktopDeepLink(s.Origin.HostSessionID)
		b.TargetTitle = desktopTitle(s.Title)
		return b, ""
	}
	if s.Origin == nil {
		return b, reasonNoOrigin
	}
	switch s.Origin.App {
	case domain.AppClaudeDesktop:
		if b.DeepLink = domain.DesktopDeepLink(s.Origin.HostSessionID); b.DeepLink == "" {
			return b, reasonNoDesktopID
		}
		b.Executor = domain.ExecutorDesktop
		b.TargetTitle = desktopTitle(s.Title)
		return b, ""
	case domain.AppTerminal:
		return terminalTarget(b, s)
	case domain.AppClient:
		// The configured client command decides how to continue; whether that
		// client is configured is checked by the executor at preflight.
		if !domain.ValidClientID(s.Origin.Client) || !domain.ValidClientSessionID(s.ID) {
			return b, reasonUnsupportedApp
		}
		b.Executor = domain.ClientExecutor(s.Origin.Client)
		b.TargetIdentifier = b.Executor + ":" + s.ID
		if domain.ValidAbsolutePath(s.Cwd) {
			b.WorkDir = s.Cwd
		}
		return b, ""
	}
	return b, reasonUnsupportedApp
}

// terminalTarget binds a claude CLI session. The terminal executor decides at
// send time, from live process facts, between its tmux pane and a headless
// `claude -p --resume`; here only the recorded prerequisites are checked.
func terminalTarget(b domain.Binding, s domain.Session) (domain.Binding, string) {
	o := s.Origin
	if !domain.ValidCLISessionID(s.ID) {
		return b, reasonUnsupportedApp
	}
	if o.ClaudePID <= 1 && !o.Detached {
		return b, reasonNoProcess
	}
	headless := o.ExecPath != "" && domain.ValidAbsolutePath(s.Cwd)
	if o.TmuxPane == "" && !headless {
		return b, reasonNoTerminalPath
	}
	b.Executor = domain.ExecutorTerminal
	b.TargetIdentifier = "claude-cli:" + s.ID
	if domain.ValidAbsolutePath(s.Cwd) {
		b.WorkDir = s.Cwd
	}
	return b, ""
}

// desktopTitle is extra on-screen identity evidence after deep-link navigation;
// titles that cannot be compared exactly are dropped rather than altered.
func desktopTitle(t string) string {
	if t == "" || len(t) > 1024 || strings.TrimSpace(t) != t || strings.IndexFunc(t, unicode.IsControl) >= 0 {
		return ""
	}
	return t
}

// plan derives the effective resume binding. Explicit legacy guard bindings keep
// their exact semantics; every other session is hosted from observed facts.
func plan(accounts []domain.Account, s domain.Session, p domain.Policy) (domain.Binding, string) {
	if legacyGuard(s) {
		if problem := bindingProblem(s); problem != "" {
			return domain.Binding{}, problem
		}
		b := *s.Binding
		b.ContinueMessage = domain.EffectiveContinueMessage(b, p)
		return b, ""
	}
	if s.Binding != nil && (s.Binding.ResumeOwner == "native" || s.Binding.ResumeOwner == "manual") {
		return domain.Binding{}, reasonLegacyNative
	}
	switch domain.EffectiveHostMode(s, p) {
	case domain.HostMonitor:
		return domain.Binding{}, reasonMonitorOnly
	case domain.HostOff:
		return domain.Binding{}, reasonHostOff
	}
	if p.AutomationConsentAt == nil {
		return domain.Binding{}, reasonNoConsent
	}
	if s.Kind != "code" {
		return domain.Binding{}, reasonNotCode
	}
	pool := resolvePool(accounts, s, p)
	if len(pool.AccountIDs) == 0 {
		return domain.Binding{}, reasonNoPool
	}
	b, problem := resolveTarget(s)
	if problem != "" {
		return domain.Binding{}, problem
	}
	message := s.ContinueMessage
	if message == "" {
		message = p.ContinueMessage
	}
	if !domain.ValidContinueMessage(message) {
		return domain.Binding{}, reasonBadMessage
	}
	b.Provider, b.Model, b.AccountIDs, b.Inferred = pool.Provider, pool.Model, pool.AccountIDs, pool.Inferred
	b.AutoResume, b.AcknowledgeSideEffects, b.ResumeOwner = true, true, "auto"
	b.Priority, b.ContinueMessage = s.Priority, message
	return b, ""
}

// jobBinding is the binding a claimed job must still match. Test jobs need only
// a target; resume jobs need a full plan whose pool still contains the account.
func jobBinding(accounts []domain.Account, s domain.Session, j domain.Job, p domain.Policy) (domain.Binding, string) {
	if j.Kind == "test" {
		b, problem := resolveTarget(s)
		if legacyGuard(s) && problem != "" {
			b, problem = *s.Binding, ""
			b.Executor = ""
		}
		b.ContinueMessage = j.Message
		return b, problem
	}
	return plan(accounts, s, p)
}

// sameExecution guards a claimed job against target, text or pool drift.
func sameExecution(claimed, current domain.Binding, j domain.Job) bool {
	if claimed.Executor != current.Executor || claimed.DeepLink != current.DeepLink || claimed.TargetIdentifier != current.TargetIdentifier || claimed.TargetTitle != current.TargetTitle || claimed.TargetOverride != current.TargetOverride || claimed.WorkDir != current.WorkDir {
		return false
	}
	if claimed.Executor == domain.ExecutorTerminal && terminalProcess(claimed.Origin) != terminalProcess(current.Origin) {
		// The session moved to another process or pane, or its process ended.
		return false
	}
	if j.MessageDigest == "" || digestText(current.ContinueMessage) != j.MessageDigest {
		return false
	}
	return j.Kind == "test" || contains(current.AccountIDs, j.AccountID)
}

func terminalProcess(o *domain.SessionOrigin) string {
	if o == nil {
		return ""
	}
	return fmt.Sprintf("%d/%t/%s/%d", o.ClaudePID, o.Detached, o.TmuxPane, o.TmuxServerPID)
}

// hostView is the snapshot explanation of the hosting decision.
func hostView(accounts []domain.Account, s domain.Session, p domain.Policy) *domain.HostView {
	mode := domain.EffectiveHostMode(s, p)
	if legacyGuard(s) {
		mode = "legacy"
	}
	b, problem := plan(accounts, s, p)
	v := &domain.HostView{Mode: mode, Executor: b.Executor, Ready: problem == "", Problem: domain.Explain(problem)}
	switch {
	case b.DeepLink != "":
		v.Target = b.DeepLink
	case b.TargetIdentifier != "":
		v.Target = b.TargetIdentifier
	case b.TargetTitle != "":
		v.Target = b.TargetTitle
	}
	if problem != "" {
		t, _ := resolveTarget(s)
		v.Executor = t.Executor
	}
	return v
}
