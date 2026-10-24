import SwiftUI

struct WorkbenchSources: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var cachedAdapters: [GuardAdapter] = []
    @State private var cachedSources: [GuardSource] = []
    private var adapters: [GuardAdapter] { runtime.snapshot?.adapters ?? cachedAdapters }
    private var sources: [GuardSource] { runtime.snapshot?.sources ?? cachedSources }
    @State private var editing = false
    @State private var draft: [String: Any] = [:]
    @State private var failure = ""
    @State private var removing: GuardSource?
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 24) {
                HStack {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(L("来源与适配", "Sources & Adapters")).font(.title.bold())
                        Text(L("额度来源、会话观察和恢复执行独立声明能力，不互相冒充。",
                               "Quota sources, session observers, and resume executors declare capabilities independently.")).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button(L("添加额度来源", "Add Quota Source")) { draft = [:]; editing = true }.buttonStyle(.borderedProminent).disabled(!runtime.daemonRunning)
                }
                if !failure.isEmpty { Text(failure).foregroundStyle(.orange).textSelection(.enabled) }
                GroupBox(L("已配置来源", "Configured Sources")) {
                    VStack(alignment: .leading, spacing: 16) {
                        if sources.isEmpty { Text(L("尚未读取到来源。连接本地后台后重新读取。", "No sources loaded yet. Re-read after connecting to daemon.")).foregroundStyle(.secondary) }
                        ForEach(sources) { source in
                            VStack(alignment: .leading, spacing: 10) {
                                HStack {
                                    Label(source.id, systemImage: source.connected ? "checkmark.circle" : "exclamationmark.circle").font(.headline)
                                    Spacer()
                                    Text(sourceState(source)).font(.caption).foregroundStyle(.secondary)
                                    Button(L("配置", "Configure")) { edit(source) }
                                    Button { removing = source } label: { Image(systemName: "trash") }.help(L("移除此来源配置；不会删除会话或数据库", "Remove this source configuration; does not delete sessions or databases"))
                                }
                                Text("\(source.type) · " + L("\(source.account_count) 个账号", "\(source.account_count) account(s)") + " · " + L("检查于 ", "Checked ") + source.checked_at).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                                if let last = source.last_success_at { Text(L("最后成功：", "Last success: ") + last).font(.caption).foregroundStyle(.secondary) }
                                if source.error?.isEmpty == false { Text(L("来源查询失败。上次结果仅保留显示，不能作为新鲜恢复证据。", "Source query failed. Last result displayed for reference; cannot serve as fresh quota evidence.")).font(.callout).foregroundStyle(.orange) }
                            }
                            if source.id != sources.last?.id { Divider() }
                        }
                        Button(L("重新读取能力与健康", "Reload Capabilities & Health")) { Task { await load() } }.disabled(!runtime.daemonRunning)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                Text(L("来源标识创建后不可改名。修改来源地址或凭据时后台会短暂重启，重启后自动恢复原来的托管开关。", "Source ID is immutable after creation. Daemon restarts briefly when changing endpoints or credentials.")).font(.caption).foregroundStyle(.secondary)
                WorkbenchSignals(signals: runtime.snapshot?.signals ?? [])
                ForEach(["quota_source", "reset_signal", "session_observer", "resume_executor"], id: \.self) { kind in
                    let kindTitlesZh = ["quota_source": "额度来源适配器", "reset_signal": "重置信号适配器", "session_observer": "会话观察器", "resume_executor": "恢复执行器"]
                    let kindTitlesEn = ["quota_source": "Quota Source Adapters", "reset_signal": "Reset Signal Adapters", "session_observer": "Session Observers", "resume_executor": "Resume Executors"]
                    GroupBox(L(kindTitlesZh[kind] ?? kind, kindTitlesEn[kind] ?? kind)) {
                        VStack(alignment: .leading, spacing: 14) {
                            ForEach(adapters.filter { $0.kind == kind }) { adapter in
                                VStack(alignment: .leading, spacing: 7) {
                                    let implZh = adapter.implemented ? (adapter.read_only ? "已实现 · 只读" : "已实现 · 需授权") : "尚未集成"
                                    let implEn = adapter.implemented ? (adapter.read_only ? "Implemented · Read-only" : "Implemented · Authorized") : "Not Integrated"
                                    HStack {
                                        Text(adapter.name).font(.headline)
                                        Spacer()
                                        Label(L(implZh, implEn), systemImage: adapter.implemented ? "checkmark.circle" : "minus.circle").font(.caption).foregroundStyle(.secondary)
                                    }
                                    if let capabilities = adapter.capabilities, !capabilities.isEmpty { Text(capabilities.joined(separator: " · ")).font(.caption.monospaced()).foregroundStyle(.secondary) }
                                    ForEach(adapter.limitations ?? [], id: \.self) { Text($0).font(.caption).foregroundStyle(.secondary) }
                                }
                                if adapter.id != adapters.filter({ $0.kind == kind }).last?.id { Divider() }
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                    }
                }
                GroupBox(L("自定义适配的安全范围", "Custom Adapter Security Boundaries")) {
                    Text(L("额度来源可通过 normalized-v1 接入只读 HTTPS／本机 HTTP 服务。其他客户端（Codex 等）只能通过配置文件 clients 中显式列出的绝对路径命令继续，参数逐个替换、不经过 shell；不会加载第三方动态库或插件。上方列为「尚未集成」的执行器不会被调用。详见 docs/EXTENDING.md。",
                           "Quota sources connect via read-only HTTPS/local HTTP normalized-v1 endpoints. External clients continue strictly via absolute commands in clients config without shell expansion. See docs/EXTENDING.md.")).frame(maxWidth: .infinity, alignment: .leading).padding(10).foregroundStyle(.secondary)
                }
            }.padding(28).frame(maxWidth: 980, alignment: .leading)
        }.task { await load() }
            .sheet(isPresented: $editing) { SourceEditor(runtime: runtime, original: draft, close: { editing = false; Task { await load() } }) }
            .confirmationDialog(L("移除 \(removing?.id ?? "")？", "Remove \(removing?.id ?? "")?"), isPresented: Binding(get: { removing != nil }, set: { if !$0 { removing = nil } })) {
                Button(L("移除来源并暂停恢复", "Remove Source & Pause"), role: .destructive) {
                    let id = removing?.id ?? ""; removing = nil
                    Task { do { try await runtime.removeSource(id); await load() } catch { failure = (error as? RuntimeFailure)?.message ?? L("来源未能移除；请检查后台状态。", "Failed to remove source; check daemon status.") } }
                }
                Button(L("返回", "Cancel"), role: .cancel) { removing = nil }
            } message: { Text(L("关联资源可能变为缺失，会话和任务记录不会删除。最后一个显式来源不能直接删除，以免静默回退至默认连接。钥匙串凭据不会被自动销毁。", "Associated resources may become missing; session records remain. The last explicit source cannot be removed directly.")) }
    }
    private func sourceState(_ source: GuardSource) -> String {
        if !runtime.localServiceFresh { return L("本地状态过期", "Stale Local State") }
        let zh = ["healthy": "健康", "unknown": "待检查", "error": "查询失败"]
        let en = ["healthy": "Healthy", "unknown": "Pending", "error": "Error"]
        return (LanguageManager.shared.isChinese ? zh : en)[source.state] ?? source.state
    }
    private func edit(_ source: GuardSource) {
        do {
            if let item = try runtime.sourceConfigurations().first(where: { $0["id"] as? String == source.id }) { draft = item; editing = true }
            else { runtime.openWorkbench(.settings); failure = "兼容默认连接请使用连接设置；添加第一个显式来源会替换兼容默认来源集合。" }
        } catch { failure = "无法读取当前来源配置；未展示私有配置。" }
    }
    private func load() async {
        do {
            cachedAdapters = try JSONDecoder().decode(GuardAdapterCatalog.self, from: await runtime.request("/api/adapters")).adapters
            cachedSources = try JSONDecoder().decode(GuardSourceCatalog.self, from: await runtime.request("/api/sources")).sources
            failure = ""
        } catch { failure = (error as? RuntimeFailure)?.message ?? "无法读取适配器能力；不会假定工具已经支持。" }
    }
}

