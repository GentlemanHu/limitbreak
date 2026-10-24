import SwiftUI

struct WorkbenchSignals: View {
    let signals: [TraySignalSource]
    var body: some View {
        GroupBox(L("重置信号", "Reset Signals")) {
            VStack(alignment: .leading, spacing: 14) {
                Text(L("第三方重置信号只会让对应账号提前重新查询额度；继续发送仍然只看最新额度证据。信号错了，最多多查一次。",
                       "Third-party reset signals only trigger earlier quota re-queries; continuing always depends on fresh quota evidence.")).font(.caption).foregroundStyle(.secondary)
                if signals.isEmpty {
                    Text(L("未启用重置信号。可在配置 signals.feeds 中添加，或通过 Webhook 推送。",
                           "No reset signals configured. Add feeds under signals.feeds or push via Webhook.")).font(.caption).foregroundStyle(.secondary)
                }
                ForEach(signals) { source in
                    WorkbenchSignalRow(source: source)
                    if source.id != signals.last?.id { Divider() }
                }
            }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
        }
    }
}

// Kept separate from WorkbenchSignals so each body stays small enough for the
// Swift type checker on slower CI machines.
private struct WorkbenchSignalRow: View {
    let source: TraySignalSource
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Label(title, systemImage: icon).font(.headline)
                Spacer()
                Text(source.provider + " · " + stateTitle).font(.caption).foregroundStyle(.secondary)
            }
            if let latest = source.latest { line(L("最近重置", "Recent Reset"), latest.at, latest.text) }
            if let scheduled = source.scheduled { line(L("预定重置", "Scheduled Reset"), scheduled.at, scheduled.text) }
            if let prediction = prediction { Text(prediction).font(.caption).foregroundStyle(.secondary) }
            if let next = nextPoll { Text(next).font(.caption).foregroundStyle(.secondary) }
            if source.error?.isEmpty == false { Text(L("信号源暂不可用，额度仍按原节奏查询。", "Signal source unavailable; querying quota on regular cadence.")).font(.caption).foregroundStyle(.orange) }
            if let name = source.attribution, let link = source.attribution_url.flatMap(URL.init(string:)) {
                Link(name, destination: link).font(.caption)
            }
        }
    }
    private var title: String { source.name.isEmpty ? source.id : source.name }
    private var prediction: String? {
        guard let watch = source.watch else { return nil }
        var text = L("预测：", "Forecast: ") + watch.level
        if let chance = watch.chance_percent { text += " · \(chance)%" }
        if let window = watch.window { text += " · " + window }
        return text + L("（第三方预测，非官方承诺）", " (Third-party forecast, not official guarantee)")
    }
    private var nextPoll: String? {
        guard let next = trayDate(source.next_poll_at) else { return nil }
        return L("下次检查：", "Next check: ") + next.formatted(date: .omitted, time: .shortened)
    }
    private func line(_ title: String, _ at: String?, _ text: String?) -> some View {
        let when = trayDate(at).map { $0.formatted(date: .abbreviated, time: .shortened) } ?? L("时间未知", "Unknown time")
        let detail = text.map { $0.isEmpty ? "" : " · " + $0 } ?? ""
        return Text(title + L("：", ": ") + when + detail).font(.caption).foregroundStyle(.secondary).lineLimit(2).textSelection(.enabled)
    }
    private var icon: String {
        if !source.enabled { return "minus.circle" }
        return ["ok": "dot.radiowaves.left.and.right", "error": "exclamationmark.circle", "idle": "pause.circle"][source.state] ?? "questionmark.circle"
    }
    private var stateTitle: String {
        if !source.enabled { return L("已关闭", "Disabled") }
        let zh = ["ok": "正常", "error": "暂不可用", "idle": "无对应账号，暂停轮询", "unknown": "待检查"]
        let en = ["ok": "Active", "error": "Unavailable", "idle": "No accounts, polling paused", "unknown": "Pending"]
        return (LanguageManager.shared.isChinese ? zh : en)[source.state] ?? source.state
    }
}
