import SwiftUI

private struct PolicyField: Identifiable {
    let id: String
    let title: String
    let bounds: ClosedRange<Int>
    var fallback = 0
}

struct WorkbenchPolicySettings: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var values: [String: String] = [:]
    @State private var original: [String: String] = [:]
    @State private var digest: String?
    @State private var busy = false
    @State private var message = ""
    @State private var expanded = false
    // Concurrency and the send interval live on the hosting-rules page.
    private let fields: [PolicyField] = [
        .init(id: "safety_seconds", title: "重置提示安全余量（秒，不替代新证据）", bounds: 0...3600),
        .init(id: "freshness_seconds", title: "证据有效期（秒）", bounds: 1...900),
        .init(id: "verify_seconds", title: "提交回执核验期（秒）", bounds: 5...300),
        .init(id: "long_probe_seconds", title: "长周期默认复查（秒）", bounds: 60...86400),
        .init(id: "unknown_probe_seconds", title: "未知额度复查（秒）", bounds: 60...86400),
        .init(id: "early_probe_seconds", title: "受限额度提前复查周期（秒）", bounds: 30...3600, fallback: 120),
        .init(id: "near_reset_probe_seconds", title: "临近／已到重置复查周期（秒）", bounds: 30...300, fallback: 30),
        .init(id: "reset_lookahead_seconds", title: "临近重置范围（秒）", bounds: 30...86400, fallback: 300),
        .init(id: "user_idle_seconds", title: "写入前用户空闲门槛（秒）", bounds: 1...3600, fallback: 3),
        .init(id: "running_stale_seconds", title: "运行失联提醒（秒，仍保留并发预算）", bounds: 60...86400, fallback: 900),
        .init(id: "queue_aging_seconds", title: "等待优先级增长周期（秒）", bounds: 1...86400, fallback: 60),
        .init(id: "max_auto_resumes_per_hour", title: "每会话每小时自动恢复上限", bounds: 1...100, fallback: 3),
    ]
    var body: some View {
        GroupBox { VStack(alignment: .leading, spacing: 0) {
            Button {
                if !expanded { load() }
                withAnimation { expanded.toggle() }
            } label: {
                HStack {
                    Image(systemName: "chevron.right").rotationEffect(.degrees(expanded ? 90 : 0))
                    Text("高级调度参数").font(.headline)
                    Spacer()
                    Text(expanded ? "收起" : "展开").font(.caption).foregroundStyle(.secondary)
                }.contentShape(Rectangle())
            }.buttonStyle(.plain).padding(.horizontal, 10).padding(.top, 8).padding(.bottom, expanded ? 0 : 8)
            if expanded { form }
        } }.onAppear { load() }
    }
    private var form: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("一般无需修改。并发与发送间隔在「托管规则」页。额度接近重置时会更频繁地复查，只有读到真实的新额度才会发送；来源自身的限流不会被绕过。").font(.caption).foregroundStyle(.secondary)
            ForEach(fields) { field in
                HStack {
                    Text(field.title).font(.callout)
                    Spacer()
                    TextField("数值", text: Binding(get: { values[field.id] ?? "" }, set: { values[field.id] = $0 }))
                        .textFieldStyle(.roundedBorder).frame(width: 90).disabled(busy)
                }
            }
            HStack {
                Button("放弃修改") { load() }.disabled(busy)
                Button(busy ? "保存中…" : "保存参数") { Task { await save() } }.buttonStyle(.borderedProminent)
                    .disabled(busy || digest == nil || !runtime.localServiceFresh)
            }
            if !message.isEmpty { Text(message).font(.caption).foregroundStyle(.secondary) }
            Text("只保存改动的字段；如果其他地方（例如 MCP）同时改过规则，会提示你重新读取，不会覆盖。").font(.caption).foregroundStyle(.secondary)
        }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
    }
    private func load() {
        guard let snapshot = runtime.snapshot else { message = "尚未取得规则"; return }
        for field in fields {
            let observed = Int(snapshot.policy[field.id]?.number ?? 0)
            values[field.id] = String(observed == 0 && field.fallback != 0 ? field.fallback : observed)
        }
        original = values
        digest = snapshot.policy_digest
        message = ""
    }
    @MainActor private func save() async {
        guard !busy else { return }
        var changes: [String: JSONValue] = [:]
        for field in fields where values[field.id] != original[field.id] {
            guard let value = Int(values[field.id] ?? ""), field.bounds.contains(value) else { message = "\(field.title)须为\(field.bounds.lowerBound)至\(field.bounds.upperBound)的整数"; return }
            changes[field.id] = .number(Double(value))
        }
        guard !changes.isEmpty else { message = "没有修改"; return }
        busy = true; defer { busy = false }
        do {
            _ = try await runtime.patchPolicy(changes, expectedDigest: digest)
            await runtime.refresh()
            load()
            message = "已保存"
        } catch { message = (error as? RuntimeFailure)?.message ?? "保存未确认，请放弃修改后重试" }
    }
}
