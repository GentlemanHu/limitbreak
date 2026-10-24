import SwiftUI

// What the guard will do for this session, in one sentence, plus the queue facts.
struct SessionStatusCard: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    let session: TraySession
    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 9) {
                HStack(alignment: .top, spacing: 12) {
                    Image(systemName: icon).font(.system(size: 26)).foregroundStyle(color).frame(width: 32)
                    VStack(alignment: .leading, spacing: 4) {
                        Text(headline).font(.title3.weight(.semibold))
                        Text(plan).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                    Spacer()
                    Text(localizedState(session.state)).font(.caption).padding(.horizontal, 7).padding(.vertical, 2).background(.quaternary, in: Capsule())
                }
                SessionClockStrip(runtime: runtime, session: session)
                if let problem = session.host?.problem, !problem.isEmpty, session.host?.mode == "hosted" {
                    HStack {
                        Label(problem, systemImage: "exclamationmark.triangle").font(.callout).foregroundStyle(.orange)
                        Spacer()
                        if !runtime.automationConsented { ConsentButton(runtime: runtime).controlSize(.small) }
                    }
                }
                if hosted && runtime.paused {
                    HStack {
                        Label(L("已紧急暂停：本会话的托管设置保留，恢复后生效。", "Emergency paused: hosting settings preserved, effective once resumed."), systemImage: "pause.octagon").font(.callout).foregroundStyle(.red)
                        Spacer()
                        EmergencyPauseButton(runtime: runtime, compact: true).controlSize(.small)
                    }
                }
                if !session.explanation.isEmpty {
                    Text(L("最近状态：", "Recent status: ") + LanguageManager.shared.explain(reason: session.reason, reasonText: session.reason_text)).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                }
                if let error = session.error, !error.isEmpty { Text(error).font(.caption).foregroundStyle(.orange).textSelection(.enabled) }
                SessionDeliveryInfo(runtime: runtime, session: session)
                if let job = runtime.job(for: session), job.kind != "test" {
                    Divider()
                    if let hint = trayDate(job.reset_hint) { LabeledContent(L("官方提示重置", "Reported Reset"), value: hint.formatted(date: .abbreviated, time: .shortened)) }
                    if let account = job.account_id, !account.isEmpty { LabeledContent(L("等待的账号", "Target Account"), value: runtime.accountName(account)) }
                    if let attempts = job.attempts, attempts > 0 { LabeledContent(L("已尝试", "Attempts"), value: L("\(attempts) 次", "\(attempts) try(s)")) }
                }
                if let native = session.native_resume, !native.isEmpty {
                    let msgZh = ["fired": "Claude 已自行自动继续，本次不再发送", "stale": "Claude 的原生自动继续已过期，交由 LimitBreak", "disabled": "Claude 原生自动继续已关闭，交由 LimitBreak"][native] ?? native
                    let msgEn = ["fired": "Claude auto-resumed natively; skipping send", "stale": "Native auto-resume timed out; handed to LimitBreak", "disabled": "Native auto-resume disabled; handed to LimitBreak"][native] ?? native
                    Text(L(msgZh, msgEn)).font(.caption).foregroundStyle(.secondary)
                }
            }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
        }
    }
    private var hosted: Bool { session.host?.mode == "hosted" }
    private var ready: Bool { session.host?.ready == true }
    private var icon: String {
        if session.state == "NEEDS_ATTENTION" { return "exclamationmark.circle.fill" }
        if ["COOLDOWN", "READY_TO_RESUME", "DISPATCHING", "VERIFYING"].contains(session.state) { return "clock.fill" }
        return hosted ? (ready ? "checkmark.seal.fill" : "exclamationmark.triangle.fill") : "eye.circle"
    }
    private var color: Color {
        if session.state == "NEEDS_ATTENTION" { return .orange }
        if ["COOLDOWN", "READY_TO_RESUME", "DISPATCHING", "VERIFYING"].contains(session.state) { return .blue }
        return hosted ? (ready ? .green : .orange) : .secondary
    }
    private var headline: String {
        switch session.state {
        case "NEEDS_ATTENTION": return L("需要你看一眼", "Needs Attention")
        case "COOLDOWN": return L("额度用完，等待恢复", "Rate Limited · Waiting for Quota")
        case "READY_TO_RESUME": return L("额度已恢复，即将继续", "Quota Restored · Resuming Soon")
        case "DISPATCHING": return L("正在发送继续文案", "Sending Continue Prompt")
        case "VERIFYING": return L("已发送，等待会话回执", "Sent · Awaiting Turn Receipt")
        default:
            if hosted { return ready ? L("智能托管 · 已就绪", "Hosted · Ready") : L("智能托管 · 暂不能自动发送", "Hosted · Auto-send Blocked") }
            return TrayHostMode.localizedTitle(session.host?.mode ?? "")
        }
    }
    private var plan: String {
        let text = session.continue_message?.isEmpty == false ? session.continue_message! : (runtime.snapshot?.policy["continue_message"]?.string ?? LanguageManager.shared.defaultContinuePrompt)
        let pool = session.pool?.account_ids?.count ?? 0
        let source = (session.host_mode ?? "").isEmpty ? L("（跟随全局默认托管）", " (Follow global defaults)") : L("（单独设置）", " (Custom setting)")
        switch session.host?.mode ?? "" {
        case "hosted":
            return L("这个会话额度用完时，LimitBreak 会记住它；\(pool > 0 ? "额度池中 \(pool) 个账号任一恢复" : "额度恢复")后，打开它并发送「\(text)」。\(source)",
                     "When rate-limited, LimitBreak remembers this session. Once \(pool > 0 ? "any of the \(pool) accounts in its pool recovers" : "quota recovers"), it will open the session and send \"\(text)\".\(source)")
        case "monitor": return L("只记录额度用完和恢复时间，不会自动发送。\(source)", "Logs rate-limit and recovery events only; will not auto-send.\(source)")
        case "off": return L("不处理这个会话。", "Does not manage this session.")
        default: return L("按旧版绑定处理。", "Handles according to legacy binding.")
        }
    }
}

struct SessionFacts: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    var body: some View {
        GroupBox(L("额度池与来源", "Quota Pool & Sources")) {
            VStack(alignment: .leading, spacing: 7) {
                LabeledContent(L("模型", "Model"), value: session.model.isEmpty ? L("尚未知", "Unknown yet") : session.model)
                LabeledContent(L("额度池", "Quota Pool"), value: (session.pool?.basisTitle ?? L("未知", "Unknown")) + (session.pool?.inferred == true ? L("（推断）", " (Inferred)") : ""))
                let accounts = session.pool?.account_ids ?? []
                Text(accounts.isEmpty ? L("没有匹配的账号：可在上方手动指定额度池，或在「托管规则」里添加模型映射", "No matching accounts: manually specify a quota pool above, or add a model alias in Hosting Rules.") : accounts.map(runtime.accountName).joined(separator: ", "))
                    .font(.callout).foregroundStyle(.secondary)
                Divider()
                LabeledContent(L("运行于", "Running on"), value: session.origin?.appTitle ?? L("未知", "Unknown"))
                LabeledContent(L("来自 Claude 侧栏", "From Claude Sidebar"), value: session.in_catalog == true ? L("是", "Yes") : L("否（仅由会话事件得知）", "No (Session hooks only)"))
                if let origin = session.origin {
                    if let entry = origin.entrypoint, !entry.isEmpty { LabeledContent(L("入口", "Entrypoint"), value: entry) }
                    if let term = origin.term_program, !term.isEmpty { LabeledContent(L("终端", "Terminal"), value: term) }
                    if let engine = origin.engine_version, !engine.isEmpty { LabeledContent(L("引擎", "Engine"), value: engine) }
                    if let host = origin.host_session_id, !host.isEmpty { LabeledContent(L("桌面会话", "Desktop Session"), value: host) }
                    if origin.app == "terminal" {
                        LabeledContent(L("tmux 窗格", "tmux Pane"), value: origin.tmux_pane?.isEmpty == false ? origin.tmux_pane! : L("不在 tmux 中", "Not in tmux"))
                        LabeledContent(L("进程", "Process"), value: origin.detached == true ? L("已关闭（将后台续写）", "Closed (Resumes in background)") : (origin.claude_pid.map { "claude \($0)" } ?? L("未采集", "Not collected")))
                        if let path = origin.exec_path, !path.isEmpty { LabeledContent(L("可执行文件", "Executable"), value: path) }
                    }
                }
                LabeledContent(L("发送目标", "Send Target"), value: session.host?.target?.isEmpty == false ? session.host!.target! : L("未确定", "Unset"))
                Text(session.id).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
            }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
        }
    }
}

struct SessionEvents: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    var body: some View {
        let items = runtime.events(for: session)
        if !items.isEmpty {
            GroupBox(L("最近记录", "Recent Events")) {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(items) { item in
                        HStack(alignment: .firstTextBaseline, spacing: 10) {
                            Text(trayDate(item.time)?.formatted(date: .omitted, time: .standard) ?? "").font(.caption.monospacedDigit()).foregroundStyle(.secondary).frame(width: 70, alignment: .leading)
                            Text(item.localized).font(.caption).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                        }
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
            }
        }
    }
}

extension TrayAudit {
    var localized: String {
        let isEN = !LanguageManager.shared.isChinese
        switch kind {
        case "hook":
            let hookZh = ["SessionStart": "会话开始", "UserPromptSubmit": "收到一条消息", "Stop": "本轮结束", "StopFailure": "本轮中断",
                          "SessionEnd": "会话进程结束", "Notification": "通知", "ModelChange": "切换模型", "PostModelSwitch": "切换模型"]
            let hookEn = ["SessionStart": "Session started", "UserPromptSubmit": "Message received", "Stop": "Round completed", "StopFailure": "Round failed",
                          "SessionEnd": "Session process exited", "Notification": "Notification", "ModelChange": "Model switched", "PostModelSwitch": "Model switched"]
            let prefix = isEN ? "Hook: " : "会话上报："
            return prefix + ((isEN ? hookEn : hookZh)[message] ?? message)
        case "bridge_result": return L("发送结果：", "Delivery result: ") + display
        case "policy": return message == "scheduling policy saved" ? L("托管规则已保存", "Hosting rules saved") : display
        case "binding": return message == "explicit binding saved" ? L("已保存会话的额度绑定", "Session quota binding saved") : display
        case "test_send": return message == "manual test send leased" ? L("手动测试发送已开始", "Manual test send leased") : display
        case "dispatch": return message.hasPrefix("leased account ") ? L("开始自动继续，使用账号 " + message.dropFirst("leased account ".count), "Auto-continue started on account " + message.dropFirst("leased account ".count)) : display
        case "attention", "cancel", "running_stale", "resume_circuit_breaker": return LanguageManager.shared.explain(reason: display)
        default: return display
        }
    }
}

extension TrayRuntime {
    func accountName(_ id: String) -> String {
        guard let account = snapshot?.accounts?.first(where: { $0.id == id }) else { return id }
        return (account.email.isEmpty ? account.id : account.email) + " · " + account.provider
    }
}
