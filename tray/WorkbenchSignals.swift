import SwiftUI

struct WorkbenchSignals: View {
    let signals: [TraySignalSource]
    var body: some View {
        GroupBox("重置信号") {
            VStack(alignment: .leading, spacing: 14) {
                Text("第三方重置信号只会让对应账号提前重新查询额度；继续发送仍然只看最新额度证据。信号错了，最多多查一次。").font(.caption).foregroundStyle(.secondary)
                if signals.isEmpty { Text("未启用重置信号。可在配置 signals.feeds 中添加，或通过 Webhook 推送。").foregroundStyle(.secondary) }
                ForEach(signals) { source in
                    VStack(alignment: .leading, spacing: 6) {
                        HStack {
                            Label(source.name.isEmpty ? source.id : source.name, systemImage: icon(source)).font(.headline)
                            Spacer()
                            Text("\(source.provider) · \(stateTitle(source))").font(.caption).foregroundStyle(.secondary)
                        }
                        if let latest = source.latest { line("最近重置", latest.at, latest.text) }
                        if let scheduled = source.scheduled { line("预定重置", scheduled.at, scheduled.text) }
                        if let watch = source.watch {
                            Text("预测：\(watch.level)" + (watch.chance_percent.map { " · \($0)%" } ?? "") + (watch.window.map { " · \($0)" } ?? "") + "（第三方预测，非官方承诺）")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        if let next = trayDate(source.next_poll_at) { Text("下次检查：" + next.formatted(date: .omitted, time: .shortened)).font(.caption).foregroundStyle(.secondary) }
                        if source.error?.isEmpty == false { Text("信号源暂不可用，额度仍按原节奏查询。").font(.caption).foregroundStyle(.orange) }
                        if let name = source.attribution, let link = source.attribution_url.flatMap(URL.init(string:)) {
                            Link(name, destination: link).font(.caption)
                        }
                    }
                    if source.id != signals.last?.id { Divider() }
                }
            }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
        }
    }
    private func line(_ title: String, _ at: String?, _ text: String?) -> some View {
        let when = trayDate(at).map { $0.formatted(date: .abbreviated, time: .shortened) } ?? "时间未知"
        return Text(title + "：" + when + (text?.isEmpty == false ? " · " + text! : "")).font(.caption).foregroundStyle(.secondary).lineLimit(2).textSelection(.enabled)
    }
    private func icon(_ source: TraySignalSource) -> String {
        if !source.enabled { return "minus.circle" }
        return ["ok": "dot.radiowaves.left.and.right", "error": "exclamationmark.circle", "idle": "pause.circle"][source.state] ?? "questionmark.circle"
    }
    private func stateTitle(_ source: TraySignalSource) -> String {
        if !source.enabled { return "已关闭" }
        return ["ok": "正常", "error": "暂不可用", "idle": "无对应账号，暂停轮询", "unknown": "待检查"][source.state] ?? source.state
    }
}
