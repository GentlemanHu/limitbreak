import SwiftUI

private struct AliasRow: Identifiable, Equatable {
    let id = UUID()
    var from: String
    var to: String
}

// Global hosting rules; every save is a CAS patch of only the changed keys.
struct WorkbenchHosting: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var mode = TrayHostMode.hosted
    @State private var message = ""
    @State private var grace = "90"
    @State private var direct = true
    @State private var resetFallback = true
    @State private var restoreFocus = true
    @State private var claudeQuiet = true
    @State private var quietSeconds = "120"
    @State private var aliases: [AliasRow] = []
    @State private var original: [String: JSONValue] = [:]
    @State private var digest: String?
    @State private var busy = false
    @State private var status = ""
    @State private var revoking = false
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                Text("托管规则").font(.title.bold())
                Text("这里是所有会话的默认值；单个会话可以在「会话托管」里单独设为智能托管、仅监控或关闭，单独设置永远优先。").foregroundStyle(.secondary)
                GroupBox("三层开关如何叠加") {
                    VStack(alignment: .leading, spacing: 6) {
                        Label("允许自动继续（一次性授权）：未授权时一律只监控。", systemImage: "1.circle")
                        Label("会话自己的托管模式；选「跟随全局」时使用下面的默认托管模式。", systemImage: "2.circle")
                        Label("紧急暂停：一键停止所有自动发送，监控与排队照常。", systemImage: "3.circle")
                    }.font(.callout).frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox("默认行为") {
                    VStack(alignment: .leading, spacing: 14) {
                        Picker("跟随全局的会话", selection: $mode) { ForEach(TrayHostMode.allCases.filter { $0 != .follow }) { Text($0.title).tag($0) } }
                        VStack(alignment: .leading, spacing: 4) {
                            TextField("默认继续文案", text: $message).textFieldStyle(.roundedBorder)
                            Text("没有单独设置文案的会话发送这段文字。必须单行。").font(.caption).foregroundStyle(.secondary)
                        }
                        HStack {
                            Text("Claude 原生自动继续的宽限期（秒）")
                            Spacer()
                            TextField("90", text: $grace).textFieldStyle(.roundedBorder).frame(width: 80)
                        }
                        Text("Claude 自己会在额度恢复时尝试继续；LimitBreak 先等这么久，确认它没有继续后再发送，避免重复。").font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox("额度查询被限流时") {
                    VStack(alignment: .leading, spacing: 8) {
                        Toggle("到记录的重置时间后尝试继续一次（推荐）", isOn: $resetFallback)
                        Text("Anthropic 的额度查询接口繁忙时会返回 429，账号显示「未知」。打开后，LimitBreak 以上次记录到的限额重置时间为准：过了重置时间加安全余量，就发送一次继续。若这次仍然撞到限额，新的失败晚于重置时间，不会再重复发送；401/403 等凭证错误永远不走这条路。关闭则一直等到查询真正确认有额度。").font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox("发送方式") {
                    VStack(alignment: .leading, spacing: 8) {
                        Toggle("后台静默直发（推荐）", isOn: $direct)
                        Text("继续文案通过 Claude 自带的会话消息端口直接送达：不切换窗口、不抢键盘，锁屏时也能发送。会话进程没有运行时（例如 Claude 里还没打开过该会话），LimitBreak 先在后台打开这个会话让 Claude 启动它的进程，最多等 20 秒，然后同样静默直发；只有进程始终没有启动，才改用界面发送（打开会话并输入）。").font(.caption).foregroundStyle(.secondary)
                        Text("若会话处于「跳过权限确认」模式，Claude 会把外来消息放进待批准列表，需要你在会话里点一次批准；LimitBreak 不会伪造权限模式，也不会因此重发。").font(.caption).foregroundStyle(.secondary)
                        Divider()
                        Toggle("打开会话后把焦点还给原来的应用（推荐）", isOn: $restoreFocus)
                        Text("后台唤醒或界面发送需要在 Claude 里打开会话时，记下你当时正在用的应用，完成后立即切回去。").font(.caption).foregroundStyle(.secondary)
                        Toggle("你正在使用 Claude 时不切换会话（推荐）", isOn: $claudeQuiet)
                        HStack {
                            Text("Claude 在最前面时，需键鼠空闲至少（秒）")
                            Spacer()
                            TextField("120", text: $quietSeconds).textFieldStyle(.roundedBorder).frame(width: 80).disabled(!claudeQuiet)
                        }
                        Text("打开会话会切换 Claude 里当前显示的会话标签。若 Claude 正在最前面且你最近操作过，LimitBreak 等你离开 Claude 或空闲这么久再唤醒或发送；会话进程已在运行时的静默直发不受影响。").font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                GroupBox("模型映射") {
                    VStack(alignment: .leading, spacing: 10) {
                        Text("会话上报的模型名与账号里的模型名不一致时，在这里对应。左侧会自动规范为小写并去掉 [1m] 之类的后缀。").font(.caption).foregroundStyle(.secondary)
                        ForEach($aliases) { $row in
                            HStack {
                                TextField("会话模型，如 claude-opus-4-8", text: $row.from).textFieldStyle(.roundedBorder)
                                Image(systemName: "arrow.right").foregroundStyle(.secondary)
                                TextField("账号模型", text: $row.to).textFieldStyle(.roundedBorder)
                                Button { aliases.removeAll { $0.id == row.id } } label: { Image(systemName: "minus.circle") }.buttonStyle(.plain)
                            }
                        }
                        HStack {
                            Button { aliases.append(AliasRow(from: "", to: "")) } label: { Label("添加映射", systemImage: "plus") }.disabled(aliases.count >= 64)
                            ForEach(unmatched.prefix(3), id: \.self) { model in
                                Button("映射 \(model)") { aliases.append(AliasRow(from: GuardPolicyPatch.normalizeModel(model), to: "")) }.font(.caption)
                            }
                        }
                        if !unmatched.isEmpty { Text("这些会话模型还没有匹配到任何账号：" + unmatched.joined(separator: "、")).font(.caption).foregroundStyle(.orange) }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
                HStack {
                    Button("放弃修改") { load() }.disabled(busy)
                    Spacer()
                    if !status.isEmpty { Text(status).font(.caption).foregroundStyle(.secondary) }
                    Button(busy ? "保存中…" : "保存规则") { Task { await save() } }.buttonStyle(.borderedProminent).disabled(busy || digest == nil)
                }
                GroupBox("自动继续授权") {
                    VStack(alignment: .leading, spacing: 10) {
                        if runtime.automationConsented {
                            Text("已允许。智能托管的会话会在额度恢复后自动发送继续文案。").font(.callout)
                            Button("撤销授权（全部改为只监控）", role: .destructive) { revoking = true }
                        } else {
                            Text("尚未允许，所有会话只监控、不自动发送。").font(.callout)
                            ConsentButton(runtime: runtime)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
            }.padding(28).frame(maxWidth: 900, alignment: .leading)
        }
        .onAppear { load() }
        .confirmationDialog("撤销后不会再自动发送任何继续文案，已排队的恢复也不会执行。", isPresented: $revoking) {
            Button("撤销授权", role: .destructive) { Task { await revoke() } }
            Button("返回", role: .cancel) {}
        }
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
        aliases = snapshot.modelAliases.sorted { $0.key < $1.key }.map { AliasRow(from: $0.key, to: $0.value) }
        original = current() ?? [:]
        digest = snapshot.policy_digest
        status = ""
    }
    private func current() -> [String: JSONValue]? {
        guard let seconds = Int(grace.trimmingCharacters(in: .whitespaces)), (1...3600).contains(seconds),
              let quiet = Int(quietSeconds.trimmingCharacters(in: .whitespaces)), (1...3600).contains(quiet) else { return nil }
        var map: [String: JSONValue] = [:]
        for row in aliases where !(row.from.isEmpty && row.to.isEmpty) {
            map[GuardPolicyPatch.normalizeModel(row.from)] = .string(row.to.trimmingCharacters(in: .whitespaces))
        }
        return ["default_host_mode": .string(mode.rawValue), "continue_message": .string(message), "native_grace_seconds": .number(Double(seconds)), "model_aliases": .object(map),
                "direct_delivery": .string(direct ? "auto" : "off"), "reset_fallback": .string(resetFallback ? "auto" : "off"),
                "restore_focus": .string(restoreFocus ? "auto" : "off"), "claude_quiet": .string(claudeQuiet ? "auto" : "off"), "claude_quiet_seconds": .number(Double(quiet))]
    }
    private func save() async {
        guard let values = current() else { status = "宽限期与 Claude 空闲秒数须为 1 至 3600 的整数"; return }
        let changes = values.filter { original[$0.key] != $0.value }
        guard !changes.isEmpty else { status = "没有修改"; return }
        busy = true; defer { busy = false }
        do {
            _ = try await runtime.patchPolicy(changes, expectedDigest: digest)
            await runtime.refresh()
            load()
            status = "已保存"
        } catch { status = (error as? RuntimeFailure)?.message ?? "保存未确认，请放弃修改后重试" }
    }
    private func revoke() async {
        await runtime.revokeAutomationConsent()
        load()
    }
}
