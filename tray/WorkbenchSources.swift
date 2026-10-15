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
                    VStack(alignment: .leading, spacing: 8) { Text("来源与适配").font(.title.bold()); Text("额度来源、会话观察和恢复执行独立声明能力，不互相冒充。").foregroundStyle(.secondary) }
                    Spacer()
                    Button("添加额度来源") { draft = [:]; editing = true }.buttonStyle(.borderedProminent).disabled(!runtime.daemonRunning)
                }
                if !failure.isEmpty { Text(failure).foregroundStyle(.orange).textSelection(.enabled) }
                GroupBox("已配置来源") {
                    VStack(alignment: .leading, spacing: 16) {
                        if sources.isEmpty { Text("尚未读取到来源。连接本地后台后重新读取。").foregroundStyle(.secondary) }
                        ForEach(sources) { source in
                            VStack(alignment: .leading, spacing: 10) {
                                HStack {
                                    Label(source.id, systemImage: source.connected ? "checkmark.circle" : "exclamationmark.circle").font(.headline)
                                    Spacer()
                                    Text(sourceState(source)).font(.caption).foregroundStyle(.secondary)
                                    Button("配置") { edit(source) }
                                    Button { removing = source } label: { Image(systemName: "trash") }.help("移除此来源配置；不会删除会话或数据库")
                                }
                                Text("\(source.type) · \(source.account_count) 个账号 · 检查于 \(source.checked_at)").font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                                if let last = source.last_success_at { Text("最后成功：" + last).font(.caption).foregroundStyle(.secondary) }
                                if source.error?.isEmpty == false { Text("来源查询失败。上次结果仅保留显示，不能作为新鲜恢复证据。").font(.callout).foregroundStyle(.orange) }
                            }
                            if source.id != sources.last?.id { Divider() }
                        }
                        Button("重新读取能力与健康") { Task { await load() } }.disabled(!runtime.daemonRunning)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                Text("来源标识创建后不可改名。修改来源地址或凭据时后台会短暂重启，重启后自动恢复原来的托管开关。").font(.caption).foregroundStyle(.secondary)
                WorkbenchSignals(signals: runtime.snapshot?.signals ?? [])
                ForEach(["quota_source", "reset_signal", "session_observer", "resume_executor"], id: \.self) { kind in
                    GroupBox(["quota_source": "额度来源适配器", "reset_signal": "重置信号适配器", "session_observer": "会话观察器", "resume_executor": "恢复执行器"][kind] ?? kind) {
                        VStack(alignment: .leading, spacing: 14) {
                            ForEach(adapters.filter { $0.kind == kind }) { adapter in
                                VStack(alignment: .leading, spacing: 7) {
                                    HStack { Text(adapter.name).font(.headline); Spacer(); Label(adapter.implemented ? (adapter.read_only ? "已实现 · 只读" : "已实现 · 需授权") : "尚未集成", systemImage: adapter.implemented ? "checkmark.circle" : "minus.circle").font(.caption).foregroundStyle(.secondary) }
                                    if let capabilities = adapter.capabilities, !capabilities.isEmpty { Text(capabilities.joined(separator: " · ")).font(.caption.monospaced()).foregroundStyle(.secondary) }
                                    ForEach(adapter.limitations ?? [], id: \.self) { Text($0).font(.caption).foregroundStyle(.secondary) }
                                }
                                if adapter.id != adapters.filter({ $0.kind == kind }).last?.id { Divider() }
                            }
                        }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                    }
                }
                GroupBox("自定义适配的安全范围") {
                    Text("额度来源可通过 normalized-v1 接入只读 HTTPS／本机 HTTP 服务。其他客户端（Codex 等）只能通过配置文件 clients 中显式列出的绝对路径命令继续，参数逐个替换、不经过 shell；不会加载第三方动态库或插件。上方列为「尚未集成」的执行器不会被调用。详见 docs/EXTENDING.md。").frame(maxWidth: .infinity, alignment: .leading).padding(10).foregroundStyle(.secondary)
                }
            }.padding(28).frame(maxWidth: 980, alignment: .leading)
        }.task { await load() }
            .sheet(isPresented: $editing) { SourceEditor(runtime: runtime, original: draft, close: { editing = false; Task { await load() } }) }
            .confirmationDialog("移除 \(removing?.id ?? "")？", isPresented: Binding(get: { removing != nil }, set: { if !$0 { removing = nil } })) {
                Button("移除来源并暂停恢复", role: .destructive) {
                    let id = removing?.id ?? ""; removing = nil
                    Task { do { try await runtime.removeSource(id); await load() } catch { failure = (error as? RuntimeFailure)?.message ?? "来源未能移除；请检查后台状态。" } }
                }
                Button("返回", role: .cancel) { removing = nil }
            } message: { Text("关联资源可能变为缺失，会话和任务记录不会删除。最后一个显式来源不能直接删除，以免静默回退至默认连接。钥匙串凭据不会被自动销毁。") }
    }
    private func sourceState(_ source: GuardSource) -> String { if !runtime.localServiceFresh { return "本地状态过期" }; return ["healthy": "健康", "unknown": "待检查", "error": "查询失败"][source.state] ?? source.state }
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

