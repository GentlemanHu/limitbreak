import SwiftUI

private struct AliasRow: Identifiable, Equatable {
    let id = UUID()
    var from: String
    var to: String
}

// Global hosting rules; every save is a CAS patch of only the changed keys.
struct WorkbenchHosting: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    @State private var mode = TrayHostMode.hosted
    @State private var message = ""
    @State private var grace = "90"
    @State private var direct = true
    @State private var resetFallback = true
    @State private var restoreFocus = true
    @State private var claudeQuiet = true
    @State private var quietSeconds = "120"
    @State private var maxRunning = "2"
    @State private var perAccount = "1"
    @State private var sendInterval = "120"
    @State private var aliases: [AliasRow] = []
    @State private var original: [String: JSONValue] = [:]
    @State private var digest: String?
    @State private var busy = false
    @State private var status = ""
    @State private var revoking = false
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                Text(L("托管规则", "Hosting Rules")).font(.title.bold())
                Text(L("这里是所有会话的默认值；单个会话可以在「会话托管」里单独设为智能托管、仅监控或关闭，单独设置永远优先。",
                       "Global defaults for all sessions. Individual sessions can be set to Hosted, Monitor Only, or Off in Sessions; custom settings always take precedence.")).foregroundStyle(.secondary)
                GroupBox(L("三层开关如何叠加", "How the Three Tiers Work")) {
                    VStack(alignment: .leading, spacing: 6) {
                        Label(L("允许自动继续（一次性授权）：未授权时一律只监控。", "Authorize auto-continue (one-time consent): monitor only until granted."), systemImage: "1.circle")
                        Label(L("会话自己的托管模式；选「跟随全局」时使用下面的默认托管模式。", "Session hosting mode: when set to 'Follow global', uses the default mode below."), systemImage: "2.circle")
                        Label(L("紧急暂停：一键停止所有自动发送，监控与排队照常。", "Emergency pause: one-click halt to all auto-sends; monitoring and queueing continue."), systemImage: "3.circle")
                    }.font(.callout).frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox(L("默认行为", "Default Behavior")) {
                    VStack(alignment: .leading, spacing: 14) {
                        Picker(L("跟随全局的会话", "Sessions following global"), selection: $mode) { ForEach(TrayHostMode.allCases.filter { $0 != .follow }) { Text($0.localizedTitle).tag($0) } }
                        VStack(alignment: .leading, spacing: 4) {
                            TextField(L("默认继续文案", "Default continue prompt"), text: $message).textFieldStyle(.roundedBorder)
                            Text(L("没有单独设置文案的会话发送这段文字。必须单行。", "Sent to sessions without a custom continue message. Must be single line.")).font(.caption).foregroundStyle(.secondary)
                        }
                        HStack {
                            Text(L("Claude 原生自动继续的宽限期（秒）", "Claude native auto-continue grace period (seconds)"))
                            Spacer()
                            TextField("90", text: $grace).textFieldStyle(.roundedBorder).frame(width: 80)
                        }
                        Text(L("Claude 自己会在额度恢复时尝试继续；LimitBreak 先等这么久，确认它没有继续后再发送，避免重复。", "Claude may attempt to auto-continue when quota recovers. LimitBreak waits this long before taking over to avoid duplicate sends.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox(L("额度查询被限流时", "When Quota Queries are Throttled")) {
                    VStack(alignment: .leading, spacing: 8) {
                        Toggle(L("到记录的重置时间后尝试继续一次（推荐）", "Try continuing once after recorded reset time (Recommended)"), isOn: $resetFallback)
                        Text(L("Anthropic 的额度查询接口繁忙时会返回 429，账号显示「未知」。打开后，LimitBreak 以上次记录到的限额重置时间为准：过了重置时间加安全余量，就发送一次继续。若这次仍然撞到限额，新的失败晚于重置时间，不会再重复发送；401/403 等凭证错误永远不走这条路。关闭则一直等到查询真正确认有额度。",
                               "When quota endpoints return HTTP 429, accounts show 'Unknown'. When enabled, LimitBreak relies on the last recorded reset time plus safety margin to attempt one continue. If still rate-limited, it will not repeat. 401/403 auth errors never trigger this fallback.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox(L("发送方式", "Delivery Methods")) {
                    VStack(alignment: .leading, spacing: 8) {
                        Toggle(L("后台静默直发（推荐）", "Silent background direct delivery (Recommended)"), isOn: $direct)
                        Text(L("继续文案通过 Claude 自带的会话消息端口直接送达：不切换窗口、不抢键盘，锁屏时也能发送。会话进程没有运行时（例如 Claude 里还没打开过该会话），LimitBreak 先在后台打开这个会话让 Claude 启动它的进程，最多等 20 秒，然后同样静默直发；只有进程始终没有启动，才改用界面发送（打开会话并输入）。",
                               "Delivers prompts directly through Claude's session socket without switching windows or stealing keyboard focus. Works while screen is locked. If session process is not running, wakes it in background first.")).font(.caption).foregroundStyle(.secondary)
                        Text(L("若会话处于「跳过权限确认」或计划模式，Claude 会扣下外来消息等你批准，所以这类会话改用界面发送；LimitBreak 不会伪造权限模式，也不会因此重发。",
                               "For sessions in bypass-permissions or plan mode, Claude holds external messages for approval, so window typing delivery is used instead.")).font(.caption).foregroundStyle(.secondary)
                        Divider()
                        Toggle(L("打开会话后把焦点还给原来的应用（推荐）", "Restore focus to original app after delivery (Recommended)"), isOn: $restoreFocus)
                        Text(L("后台唤醒或界面发送需要在 Claude 里打开会话时，记下你当时正在用的应用，完成后立即切回去。", "When opening a session window, remembers the frontmost application and restores focus immediately after.")).font(.caption).foregroundStyle(.secondary)
                        Toggle(L("你正在使用 Claude 时不切换会话（推荐）", "Do not switch sessions while Claude is in use (Recommended)"), isOn: $claudeQuiet)
                        HStack {
                            Text(L("Claude 在最前面时，需键鼠空闲至少（秒）", "When Claude is frontmost, wait for keyboard/mouse idle (seconds)"))
                            Spacer()
                            TextField("120", text: $quietSeconds).textFieldStyle(.roundedBorder).frame(width: 80).disabled(!claudeQuiet)
                        }
                        Text(L("打开会话会切换 Claude 里当前显示的会话标签。若 Claude 正在最前面且你最近操作过，LimitBreak 等你离开 Claude 或空闲这么久再唤醒或发送；会话进程已在运行时的静默直发不受影响。",
                               "Opening a session switches the visible Claude tab. If Claude is in use, LimitBreak waits until idle before waking or typing.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox(L("并发与发送节奏", "Concurrency & Pacing")) {
                    VStack(alignment: .leading, spacing: 8) {
                        numberRow(L("全局并发（同时运行的会话数）", "Global concurrency (max running sessions)"), text: $maxRunning)
                        numberRow(L("每账号并发（同一账号同时运行的会话数）", "Per-account concurrency (sessions per account)"), text: $perAccount)
                        numberRow(L("两次发送的最小间隔（秒）", "Minimum send interval (seconds)"), text: $sendInterval)
                        Text(L("多个会话同时到点也不会互相冲突：每次只发送一个，界面发送全程独占 Claude 窗口，两次发送之间至少隔上面的秒数。用同一账号的会话超过「每账号并发」时排队，前一个本回合结束后再继续；调高它会让同一账号的额度被同时消耗。",
                               "Multiple sessions reaching reset time simultaneously will never collide: only one session sends at a time, window delivery holds an exclusive desktop lease, and sends are spaced by the minimum interval.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox(L("模型映射", "Model Aliases")) {
                    VStack(alignment: .leading, spacing: 10) {
                        Text(L("会话上报的模型名与账号里的模型名不一致时，在这里对应。左侧会自动规范为小写并去掉 [1m] 之类的后缀。", "Maps session reported model names to provider account models. Left side is automatically lowercased and stripped of suffixes like [1m].")).font(.caption).foregroundStyle(.secondary)
                        ForEach($aliases) { $row in
                            HStack {
                                TextField(L("会话模型，如 claude-opus-4-8", "Session model, e.g. claude-opus-4-8"), text: $row.from).textFieldStyle(.roundedBorder)
                                Image(systemName: "arrow.right").foregroundStyle(.secondary)
                                TextField(L("账号模型", "Account model"), text: $row.to).textFieldStyle(.roundedBorder)
                                Button { aliases.removeAll { $0.id == row.id } } label: { Image(systemName: "minus.circle") }.buttonStyle(.plain)
                            }
                        }
                        HStack {
                            Button { aliases.append(AliasRow(from: "", to: "")) } label: { Label(L("添加映射", "Add Alias"), systemImage: "plus") }.disabled(aliases.count >= 64)
                            ForEach(unmatched.prefix(3), id: \.self) { model in
                                Button(L("映射 \(model)", "Map \(model)")) { aliases.append(AliasRow(from: GuardPolicyPatch.normalizeModel(model), to: "")) }.font(.caption)
                            }
                        }
                        if !unmatched.isEmpty { Text(L("这些会话模型还没有匹配到任何账号：", "Unmatched session models: ") + unmatched.joined(separator: ", ")).font(.caption).foregroundStyle(.orange) }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                HStack {
                    Button(L("放弃修改", "Discard Changes")) { load() }.disabled(busy)
                    Spacer()
                    if !status.isEmpty { Text(status).font(.caption).foregroundStyle(.secondary) }
                    Button(busy ? L("保存中…", "Saving…") : L("保存规则", "Save Rules")) { Task { await save() } }.buttonStyle(.borderedProminent).disabled(busy || digest == nil)
                }
                GroupBox(L("自动继续授权", "Auto-Continue Authorization")) {
                    VStack(alignment: .leading, spacing: 10) {
                        if runtime.automationConsented {
                            Text(L("已允许。智能托管的会话会在额度恢复后自动发送继续文案。", "Authorized. Hosted sessions will automatically continue when quota recovers.")).font(.callout)
                            Button(L("撤销授权（全部改为只监控）", "Revoke Authorization (Monitor Only)"), role: .destructive) { revoking = true }
                        } else {
                            Text(L("尚未允许，所有会话只监控、不自动发送。", "Not authorized yet; all sessions monitor only and will not auto-send.")).font(.callout)
                            ConsentButton(runtime: runtime)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
            }.padding(28).frame(maxWidth: 900, alignment: .leading)
        }
        .onAppear { load() }
        .confirmationDialog(L("撤销后不会再自动发送任何继续文案，已排队的恢复也不会执行。", "Revoking stops all automatic continue sends; queued recoveries will not execute."), isPresented: $revoking) {
            Button(L("撤销授权", "Revoke Authorization"), role: .destructive) { Task { await revoke() } }
            Button(L("返回", "Back"), role: .cancel) {}
        }
    }
    private func numberRow(_ title: String, text: Binding<String>) -> some View {
        HStack {
            Text(title)
            Spacer()
            TextField("数值", text: text).textFieldStyle(.roundedBorder).frame(width: 80)
        }
    }
    private func policyNumber(_ snapshot: TraySnapshot, _ key: String, fallback: Int) -> String {
        let observed = Int(snapshot.policy[key]?.number ?? 0)
        return String(observed == 0 ? fallback : observed)
    }
    private var unmatched: [String] {
        Array(Set(runtime.sessions.filter { !$0.model.isEmpty && $0.pool?.basis == "none" }.map(\.model))).sorted()
    }
    private func load() {
        guard let snapshot = runtime.snapshot else { status = "尚未取得策略"; return }
        mode = TrayHostMode(rawValue: snapshot.defaultHostMode).flatMap { $0 == .follow ? nil : $0 } ?? .hosted
        message = snapshot.policy["continue_message"]?.string ?? ""
        let observed = Int(snapshot.policy["native_grace_seconds"]?.number ?? 0)
        grace = String(observed == 0 ? 90 : observed)
        direct = snapshot.policy["direct_delivery"]?.string != "off"
        resetFallback = snapshot.policy["reset_fallback"]?.string != "off"
        restoreFocus = snapshot.policy["restore_focus"]?.string != "off"
        claudeQuiet = snapshot.policy["claude_quiet"]?.string != "off"
        let quiet = Int(snapshot.policy["claude_quiet_seconds"]?.number ?? 0)
        quietSeconds = String(quiet == 0 ? 120 : quiet)
        maxRunning = policyNumber(snapshot, "max_running", fallback: 2)
        perAccount = policyNumber(snapshot, "max_per_account", fallback: 1)
        sendInterval = policyNumber(snapshot, "min_send_interval_seconds", fallback: 120)
        aliases = snapshot.modelAliases.sorted { $0.key < $1.key }.map { AliasRow(from: $0.key, to: $0.value) }
        original = current() ?? [:]
        digest = snapshot.policy_digest
        status = ""
    }
    private func current() -> [String: JSONValue]? {
        guard let seconds = Int(grace.trimmingCharacters(in: .whitespaces)), (1...3600).contains(seconds),
              let quiet = Int(quietSeconds.trimmingCharacters(in: .whitespaces)), (1...3600).contains(quiet),
              let running = Int(maxRunning.trimmingCharacters(in: .whitespaces)), (1...16).contains(running),
              let account = Int(perAccount.trimmingCharacters(in: .whitespaces)), (1...16).contains(account),
              let interval = Int(sendInterval.trimmingCharacters(in: .whitespaces)), (30...86400).contains(interval) else { return nil }
        var map: [String: JSONValue] = [:]
        for row in aliases where !(row.from.isEmpty && row.to.isEmpty) {
            map[GuardPolicyPatch.normalizeModel(row.from)] = .string(row.to.trimmingCharacters(in: .whitespaces))
        }
        return ["default_host_mode": .string(mode.rawValue), "continue_message": .string(message), "native_grace_seconds": .number(Double(seconds)), "model_aliases": .object(map),
                "direct_delivery": .string(direct ? "auto" : "off"), "reset_fallback": .string(resetFallback ? "auto" : "off"),
                "restore_focus": .string(restoreFocus ? "auto" : "off"), "claude_quiet": .string(claudeQuiet ? "auto" : "off"), "claude_quiet_seconds": .number(Double(quiet)),
                "max_running": .number(Double(running)), "max_per_account": .number(Double(account)), "min_send_interval_seconds": .number(Double(interval))]
    }
    private func save() async {
        guard let values = current() else { status = L("宽限期与 Claude 空闲秒数须为 1 至 3600、并发须为 1 至 16、发送间隔须为 30 至 86400 的整数", "Grace period and idle seconds must be 1-3600, concurrency 1-16, send interval 30-86400"); return }
        let changes = values.filter { original[$0.key] != $0.value }
        guard !changes.isEmpty else { status = L("没有修改", "No changes"); return }
        busy = true; defer { busy = false }
        do {
            _ = try await runtime.patchPolicy(changes, expectedDigest: digest)
            await runtime.refresh()
            load()
            status = L("已保存", "Saved")
        } catch { status = (error as? RuntimeFailure)?.message ?? L("保存未确认，请放弃修改后重试", "Save unconfirmed; please discard and retry") }
    }
    private func revoke() async {
        await runtime.revokeAutomationConsent()
        load()
    }
}
