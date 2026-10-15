import SwiftUI

// What the guard will do for this session, in one sentence, plus the queue facts.
struct SessionStatusCard: View {
    @ObservedObject var runtime: TrayRuntime
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
                    Text(trayState(session.state)).font(.caption).padding(.horizontal, 7).padding(.vertical, 2).background(.quaternary, in: Capsule())
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
                        Label("已紧急暂停：本会话的托管设置保留，恢复后生效。", systemImage: "pause.octagon").font(.callout).foregroundStyle(.red)
                        Spacer()
                        EmergencyPauseButton(runtime: runtime, compact: true).controlSize(.small)
                    }
                }
                if !session.explanation.isEmpty {
                    Text("最近状态：" + session.explanation).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                }
                if let error = session.error, !error.isEmpty { Text(error).font(.caption).foregroundStyle(.orange).textSelection(.enabled) }
                SessionDeliveryInfo(runtime: runtime, session: session)
                if let job = runtime.job(for: session), job.kind != "test" {
                    Divider()
                    if let hint = trayDate(job.reset_hint) { LabeledContent("官方提示重置", value: hint.formatted(date: .abbreviated, time: .shortened)) }
                    if let account = job.account_id, !account.isEmpty { LabeledContent("等待的账号", value: runtime.accountName(account)) }
                    if let attempts = job.attempts, attempts > 0 { LabeledContent("已尝试", value: "\(attempts) 次") }
                }
                if let native = session.native_resume, !native.isEmpty {
                    Text(["fired": "Claude 已自行自动继续，本次不再发送", "stale": "Claude 的原生自动继续已过期，交由 LimitBreak", "disabled": "Claude 原生自动继续已关闭，交由 LimitBreak"][native] ?? native).font(.caption).foregroundStyle(.secondary)
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
        case "NEEDS_ATTENTION": return "需要你看一眼"
        case "COOLDOWN": return "额度用完，等待恢复"
        case "READY_TO_RESUME": return "额度已恢复，即将继续"
        case "DISPATCHING": return "正在发送继续文案"
        case "VERIFYING": return "已发送，等待会话回执"
        default:
            if hosted { return ready ? "智能托管 · 已就绪" : "智能托管 · 暂不能自动发送" }
            return TrayHostMode.title(session.host?.mode ?? "")
        }
    }
    private var plan: String {
        let text = session.continue_message?.isEmpty == false ? session.continue_message! : (runtime.snapshot?.policy["continue_message"]?.string ?? "继续")
        let pool = session.pool?.account_ids?.count ?? 0
        let source = (session.host_mode ?? "").isEmpty ? "（跟随全局默认托管）" : "（单独设置）"
        switch session.host?.mode ?? "" {
        case "hosted":
            return "这个会话额度用完时，LimitBreak 会记住它；\(pool > 0 ? "额度池中 \(pool) 个账号任一恢复" : "额度恢复")后，打开它并发送「\(text)」。\(source)"
        case "monitor": return "只记录额度用完和恢复时间，不会自动发送。\(source)"
        case "off": return "不处理这个会话。"
        default: return "按旧版绑定处理。"
        }
    }
}

struct SessionFacts: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    var body: some View {
        GroupBox("额度池与来源") {
            VStack(alignment: .leading, spacing: 7) {
                LabeledContent("模型", value: session.model.isEmpty ? "尚未知" : session.model)
                LabeledContent("额度池", value: (session.pool?.basisTitle ?? "未知") + (session.pool?.inferred == true ? "（推断）" : ""))
                let accounts = session.pool?.account_ids ?? []
                Text(accounts.isEmpty ? "没有匹配的账号：可在上方手动指定额度池，或在「托管规则」里添加模型映射" : accounts.map(runtime.accountName).joined(separator: "、"))
                    .font(.callout).foregroundStyle(.secondary)
                Divider()
                LabeledContent("运行于", value: session.origin?.appTitle ?? "未知")
                LabeledContent("来自 Claude 侧栏", value: session.in_catalog == true ? "是" : "否（仅由会话事件得知）")
                if let origin = session.origin {
                    if let entry = origin.entrypoint, !entry.isEmpty { LabeledContent("入口", value: entry) }
                    if let term = origin.term_program, !term.isEmpty { LabeledContent("终端", value: term) }
                    if let engine = origin.engine_version, !engine.isEmpty { LabeledContent("引擎", value: engine) }
                    if let host = origin.host_session_id, !host.isEmpty { LabeledContent("桌面会话", value: host) }
                    if origin.app == "terminal" {
                        LabeledContent("tmux 窗格", value: origin.tmux_pane?.isEmpty == false ? origin.tmux_pane! : "不在 tmux 中")
                        LabeledContent("进程", value: origin.detached == true ? "已关闭（将后台续写）" : (origin.claude_pid.map { "claude \($0)" } ?? "未采集"))
                        if let path = origin.exec_path, !path.isEmpty { LabeledContent("可执行文件", value: path) }
                    }
                }
                LabeledContent("发送目标", value: session.host?.target?.isEmpty == false ? session.host!.target! : "未确定")
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
            GroupBox("最近记录") {
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
    // The daemon logs a few fixed English phrases; everything else is already Chinese or a reason code.
    var localized: String {
        switch kind {
        case "hook":
            return "会话上报：" + (["SessionStart": "会话开始", "UserPromptSubmit": "收到一条消息", "Stop": "本轮结束", "StopFailure": "本轮中断",
                                  "SessionEnd": "会话进程结束", "Notification": "通知", "ModelChange": "切换模型", "PostModelSwitch": "切换模型"][message] ?? message)
        case "bridge_result": return "发送结果：" + display
        case "policy": return message == "scheduling policy saved" ? "托管规则已保存" : display
        case "binding": return message == "explicit binding saved" ? "已保存会话的额度绑定" : display
        case "test_send": return message == "manual test send leased" ? "手动测试发送已开始" : display
        case "dispatch": return message.hasPrefix("leased account ") ? "开始自动继续，使用账号 " + message.dropFirst("leased account ".count) : display
        case "attention", "cancel", "running_stale", "resume_circuit_breaker": return workbenchReason(display)
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
