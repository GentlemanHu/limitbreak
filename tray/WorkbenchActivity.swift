import SwiftUI

// Read-only audit trail: what was sent, skipped or changed, and why.
struct WorkbenchActivity: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var search = ""
    @State private var onlySends = false
    private static let sendKinds: Set<String> = ["dispatch", "bridge_result", "test_send", "native_resume", "attention", "resume_circuit_breaker"]
    private var items: [TrayAudit] {
        (runtime.snapshot?.audit ?? []).filter { item in
            (!onlySends || Self.sendKinds.contains(item.kind)) &&
            (search.isEmpty || "\(item.message) \(title(item.session_id)) \(kindTitle(item.kind))".localizedCaseInsensitiveContains(search))
        }.sorted { $0.id > $1.id }
    }
    var body: some View {
        VStack(spacing: 0) {
            HStack {
                TextField("搜索会话或内容", text: $search).textFieldStyle(.roundedBorder)
                Toggle("只看发送相关", isOn: $onlySends).toggleStyle(.checkbox)
            }.padding(16)
            Divider()
            if items.isEmpty {
                WorkbenchEmpty(title: "暂无记录", detail: "会话事件、发送结果和设置修改都会记录在这里。", icon: "list.bullet.rectangle")
            } else {
                List(items) { item in
                    HStack(alignment: .top, spacing: 12) {
                        Image(systemName: icon(item.kind)).foregroundStyle(color(item.kind)).frame(width: 18)
                        VStack(alignment: .leading, spacing: 3) {
                            HStack {
                                Text(kindTitle(item.kind)).font(.callout.weight(.medium))
                                if !item.session_id.isEmpty { Text(title(item.session_id)).font(.callout).foregroundStyle(.secondary).lineLimit(1) }
                                Spacer()
                                Text(trayDate(item.time)?.formatted(date: .abbreviated, time: .standard) ?? item.time).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                            }
                            Text(item.message).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                        }
                    }.padding(.vertical, 3)
                }
            }
        }
    }
    private func title(_ id: String) -> String {
        runtime.sessions.first { $0.id == id }?.displayTitle ?? String(id.prefix(8))
    }
    private func kindTitle(_ kind: String) -> String {
        ["dispatch": "发送继续", "bridge_result": "发送结果", "test_send": "测试发送", "native_resume": "原生自动继续", "attention": "需要处理",
         "resume_circuit_breaker": "自动恢复熔断", "running_stale": "运行失联", "cancel": "停止恢复", "hook": "会话事件",
         "policy": "规则修改", "session_settings": "会话设置", "binding": "旧版绑定"][kind] ?? kind
    }
    private func icon(_ kind: String) -> String {
        switch kind {
        case "dispatch", "test_send": return "paperplane"
        case "bridge_result": return "checkmark.message"
        case "attention", "resume_circuit_breaker", "running_stale": return "exclamationmark.triangle"
        case "hook": return "bolt"
        case "cancel": return "stop.circle"
        default: return "gearshape"
        }
    }
    private func color(_ kind: String) -> Color {
        ["attention", "resume_circuit_breaker", "running_stale"].contains(kind) ? .orange : ["dispatch", "test_send", "bridge_result"].contains(kind) ? .blue : .secondary
    }
}
