import SwiftUI

private enum TargetChoice: String, CaseIterable, Identifiable {
    case follow, desktop
    var id: String { rawValue }
    var title: String {
        switch self {
        case .follow: return L("跟随来源（自动）", "Follow Source (Auto)")
        case .desktop: return L("Claude 桌面", "Claude Desktop")
        }
    }
}

// Edits only what the user changed; untouched fields are not sent, so a concurrent MCP edit is not overwritten.
struct SessionEditor: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
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
        GroupBox(L("这个会话的托管设置", "Hosting Settings for this Session")) {
            VStack(alignment: .leading, spacing: 14) {
                Picker(L("托管方式", "Hosting Mode"), selection: $mode) { ForEach(TrayHostMode.allCases) { Text(modeTitle($0)).tag($0) } }
                VStack(alignment: .leading, spacing: 4) {
                    TextField(L("继续文案（留空使用全局：\(globalMessage)）", "Continue prompt (leave blank to use global: \(globalMessage))"), text: $message).textFieldStyle(.roundedBorder)
                    if let problem = draft.problem { Text(problem).font(.caption).foregroundStyle(.orange) }
                }
                VStack(alignment: .leading, spacing: 6) {
                    Toggle(L("手动指定额度池（默认按模型自动匹配）", "Manually specify quota pool (defaults to model auto-match)"), isOn: $manualPool)
                    if manualPool {
                        let accounts = runtime.snapshot?.accounts ?? []
                        if accounts.isEmpty { Text(L("还没有读取到账号", "No accounts found")).font(.caption).foregroundStyle(.secondary) }
                        ForEach(accounts) { account in
                            Toggle(isOn: Binding(get: { pool.contains(account.id) }, set: { if $0 { pool.insert(account.id) } else { pool.remove(account.id) } })) {
                                HStack(spacing: 6) {
                                    ProviderIcon(provider: account.provider, size: 16)
                                    Text((account.email.isEmpty ? account.id : account.email) + " · " + ProviderStyle(account.provider).name)
                                    if !session.model.isEmpty && !(account.models ?? []).contains(session.model) {
                                        Text(L("映射/未声明模型", "Mapped/Undeclared Model")).font(.caption2).foregroundStyle(.secondary).padding(.horizontal, 5).background(.quaternary, in: Capsule())
                                            .help(L("来源没有声明 \(session.model)。手动指定时不校验模型：只用这个账号的额度判断何时恢复（适合 CLIProxyAPI 把这个模型名映射到该账号的情况）。",
                                                    "Source does not declare \(session.model). Manual assignment bypasses model check: uses this account's quota for recovery."))
                                    }
                                }
                            }.toggleStyle(.checkbox).font(.callout)
                        }
                        Text(L("任一选中账号的额度恢复即可继续。手动指定不校验模型名，适合映射模型；只决定「等谁的额度」，不会改动会话在 Claude 里使用的模型，恢复后只发送继续文案。",
                               "Recovers when any selected account has available quota. Bypasses model matching; only determines which quota to wait on without altering the session model in Claude.")).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                VStack(alignment: .leading, spacing: 6) {
                    Picker(L("发送目标", "Send Target"), selection: $target) { ForEach(TargetChoice.allCases) { Text($0.title).tag($0) } }
                    if session.origin?.app == "terminal" {
                        Text(L("终端会话：在 tmux 里开着时直接输入到它的窗格；不在 tmux 里开着时等你关闭它，之后在后台用 claude -p --resume 续写（后台运行时请勿重新打开该会话）。",
                               "Terminal session: types directly into tmux pane when open; resumes with claude -p --resume in background when closed.")).font(.caption).foregroundStyle(.secondary)
                    }
                }
                Stepper(L("排队优先级：\(priority)（多个会话同时恢复时，高者先发）", "Queue priority: \(priority) (higher priority sent first)"), value: $priority, in: -100...100, step: 5)
                HStack {
                    Button(L("还原", "Reset")) { load() }.disabled(saving || draft == baseline)
                    Spacer()
                    Button(saving ? L("保存中…", "Saving…") : L("保存", "Save")) { Task { await save() } }.buttonStyle(.borderedProminent).keyboardShortcut("s")
                        .disabled(saving || draft == baseline || draft.problem != nil)
                }
            }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
        }.onAppear { load() }
    }
    private var globalMessage: String { runtime.snapshot?.policy["continue_message"]?.string ?? LanguageManager.shared.defaultContinuePrompt }
    private func modeTitle(_ value: TrayHostMode) -> String {
        guard value == .follow else { return value.localizedTitle }
        let currentMode = runtime.snapshot?.defaultHostMode.isEmpty == false ? runtime.snapshot!.defaultHostMode : "hosted"
        return L("跟随全局（当前：" + TrayHostMode.localizedTitle(currentMode) + "）",
                 "Follow Global (Current: " + TrayHostMode.localizedTitle(currentMode) + ")")
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
        if manualPool && pool.isEmpty { runtime.error = L("手动额度池至少选择一个账号，或关闭手动指定", "Select at least one account for manual pool or disable it"); return }
        saving = true; defer { saving = false }
        if await runtime.updateSession(session, patch), let fresh = runtime.sessions.first(where: { $0.id == session.id }) {
            mode = TrayHostMode(rawValue: fresh.host_mode ?? "") ?? .follow
            baseline = draft
        }
    }
}
