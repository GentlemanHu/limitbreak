import SwiftUI

// Every window of one account, grouped by what each limit actually restricts.
struct TrayQuotaCard: View {
    @ObservedObject var runtime: TrayRuntime
    let account: TrayAccount
    var body: some View {
        let badge = runtime.accountBadge(account)
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("额度窗口").font(.headline)
                Spacer()
                QuotaStateBadge(text: badge.0, color: badge.1)
            }
            if let error = account.quota_error, !error.isEmpty {
                Label("采集未完整：" + workbenchReason(error), systemImage: "exclamationmark.triangle").font(.caption).foregroundStyle(.orange)
            }
            if let groups = account.groups, !groups.isEmpty {
                ForEach(groups) { group in
                    VStack(alignment: .leading, spacing: 8) {
                        Divider()
                        HStack(spacing: 6) {
                            Image(systemName: group.scope == "models" ? "cpu" : group.scope == "operation" ? "text.magnifyingglass" : group.scope == "account" ? "person.crop.circle" : "questionmark.circle").foregroundStyle(.secondary)
                            Text(group.name).font(.callout.weight(.semibold))
                            Spacer()
                            Text(runtime.isFresh(group) ? trayState(group.state) : "等待复查").font(.caption).foregroundStyle(group.state == "limited" ? .red : .secondary)
                        }
                        Text(runtime.groupScopeText(group)).font(.caption2).foregroundStyle(.secondary)
                        ForEach(Array((group.windows ?? []).enumerated()), id: \.offset) { _, window in QuotaBar(window: window, placeholder: "", now: runtime.now) }
                        if group.windows?.isEmpty != false { Text("来源没有提供该组的窗口明细").font(.caption2).foregroundStyle(.secondary) }
                    }
                }
            } else {
                ForEach(Array((account.windows ?? []).enumerated()), id: \.offset) { _, window in QuotaBar(window: window, placeholder: "", now: runtime.now) }
                if account.windows?.isEmpty != false { Text(account.reason.isEmpty ? "额度尚未取得，可先绑定监控。" : workbenchReason(account.reason)).font(.caption).foregroundStyle(.secondary) }
            }
            if let metrics = account.metrics, !metrics.isEmpty {
                DisclosureGroup("积分、财务与用量明细（\(metrics.count)）") {
                    ForEach(Array(metrics.enumerated()), id: \.offset) { _, metric in
                        HStack(alignment: .top) { Text(metric.label); Spacer(); Text(metric.value.isEmpty ? "未知" : metric.value + (metric.currency.map { " " + $0 } ?? metric.unit.map { " " + $0 } ?? "")) }.font(.caption)
                    }
                }.font(.caption)
            }
        }.padding(14).background(.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 12))
    }
}
