import SwiftUI

private enum TargetChoice: String, CaseIterable, Identifiable {
    case follow, desktop
    var id: String { rawValue }
    var title: String { switch self { case .follow: return "跟随来源（自动）"; case .desktop: return "Claude 桌面" } }
}

// Edits only what the user changed; untouched fields are not sent, so a concurrent MCP edit is not overwritten.
struct SessionEditor: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    @State private var mode = TrayHostMode.follow
    @State private var message = ""
    @State private var manualPool = false
    @State private var pool = Set<String>()
    @State private var target = TargetChoice.follow
    @State private var priority = 0
    @State private var baseline = TraySessionPatch()
    @State private var saving = false
    var body: some View {
        GroupBox("这个会话的托管设置") {
            VStack(alignment: .leading, spacing: 14) {
                Picker("托管方式", selection: $mode) { ForEach(TrayHostMode.allCases) { Text(modeTitle($0)).tag($0) } }
                VStack(alignment: .leading, spacing: 4) {
                    TextField("继续文案（留空使用全局：\(globalMessage)）", text: $message).textFieldStyle(.roundedBorder)
                    if let problem = draft.problem { Text(problem).font(.caption).foregroundStyle(.orange) }
                }
                VStack(alignment: .leading, spacing: 6) {
                    Toggle("手动指定额度池（默认按模型自动匹配）", isOn: $manualPool)
                    if manualPool {
                        let accounts = runtime.snapshot?.accounts ?? []
                        if accounts.isEmpty { Text("还没有读取到账号").font(.caption).foregroundStyle(.secondary) }
                        ForEach(accounts) { account in
                            Toggle(isOn: Binding(get: { pool.contains(account.id) }, set: { if $0 { pool.insert(account.id) } else { pool.remove(account.id) } })) {
                                HStack(spacing: 6) {
                                    ProviderIcon(provider: account.provider, size: 16)
                                    Text((account.email.isEmpty ? account.id : account.email) + " · " + ProviderStyle(account.provider).name)
                                    if !session.model.isEmpty && !(account.models ?? []).contains(session.model) {
                                        Text("映射/未声明模型").font(.caption2).foregroundStyle(.secondary).padding(.horizontal, 5).background(.quaternary, in: Capsule())
                                            .help("来源没有声明 \(session.model)。手动指定时不校验模型：只用这个账号的额度判断何时恢复（适合 CLIProxyAPI 把这个模型名映射到该账号的情况）。")
                                    }
                                }
                            }.toggleStyle(.checkbox).font(.callout)
                        }
                        Text("任一选中账号的额度恢复即可继续。手动指定不校验模型名，适合映射模型；只决定「等谁的额度」，不会改动会话在 Claude 里使用的模型，恢复后只发送继续文案。").font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                VStack(alignment: .leading, spacing: 6) {
                    Picker("发送目标", selection: $target) { ForEach(TargetChoice.allCases) { Text($0.title).tag($0) } }
                    if session.origin?.app == "terminal" { Text("终端会话：在 tmux 里开着时直接输入到它的窗格；不在 tmux 里开着时等你关闭它，之后在后台用 claude -p --resume 续写（后台运行时请勿重新打开该会话）。").font(.caption).foregroundStyle(.secondary) }
                }
                Stepper("排队优先级：\(priority)（多个会话同时恢复时，高者先发）", value: $priority, in: -100...100, step: 5)
                HStack {
                    Button("还原") { load() }.disabled(saving || draft == baseline)
                    Spacer()
                    Button(saving ? "保存中…" : "保存") { Task { await save() } }.buttonStyle(.borderedProminent).keyboardShortcut("s")
                        .disabled(saving || draft == baseline || draft.problem != nil)
                }
            }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
        }.onAppear { load() }
    }
    private var globalMessage: String { runtime.snapshot?.policy["continue_message"]?.string ?? "继续" }
    private func modeTitle(_ value: TrayHostMode) -> String {
        guard value == .follow else { return value.title }
        return "跟随全局（当前：" + TrayHostMode.title(runtime.snapshot?.defaultHostMode.isEmpty == false ? runtime.snapshot!.defaultHostMode : "hosted") + "）"
    }
    // Full desired state; diffed against the loaded baseline when saving.
    private var draft: TraySessionPatch {
        TraySessionPatch(host_mode: mode.rawValue, continue_message: message.trimmingCharacters(in: .whitespaces),
                         pool_override: manualPool ? pool.sorted() : [], target_override: target == .desktop ? "desktop" : "",
                         priority: priority)
    }
    private func load() {
        mode = TrayHostMode(rawValue: session.host_mode ?? "") ?? .follow
        message = session.continue_message ?? ""
        pool = Set(session.pool_override ?? [])
        manualPool = !pool.isEmpty
        target = session.target_override == "desktop" ? .desktop : .follow
        priority = session.priority ?? 0
        baseline = draft
    }
    private func save() async {
        let current = draft
        var patch = TraySessionPatch()
        if current.host_mode != baseline.host_mode { patch.host_mode = current.host_mode }
        if current.continue_message != baseline.continue_message { patch.continue_message = current.continue_message }
        if current.pool_override != baseline.pool_override { patch.pool_override = current.pool_override }
        if current.target_override != baseline.target_override { patch.target_override = current.target_override }
        if current.priority != baseline.priority { patch.priority = current.priority }
        if manualPool && pool.isEmpty { runtime.error = "手动额度池至少选择一个账号，或关闭手动指定"; return }
        saving = true; defer { saving = false }
        if await runtime.updateSession(session, patch), let fresh = runtime.sessions.first(where: { $0.id == session.id }) {
            mode = TrayHostMode(rawValue: fresh.host_mode ?? "") ?? .follow
            baseline = draft
        }
    }
}
