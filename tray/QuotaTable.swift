import SwiftUI

// The same accounts as the tile grid, one row each, for many accounts at a glance.
struct QuotaTable: View {
    @ObservedObject var runtime: TrayRuntime
    let accounts: [TrayAccount]
    let open: (String) -> Void
    @State private var selection = Set<String>()
    var body: some View {
        Table(accounts, selection: $selection) {
            TableColumn(L("账号", "Account")) { account in
                HStack(spacing: 8) {
                    ProviderIcon(provider: account.provider, size: 22)
                    VStack(alignment: .leading, spacing: 1) {
                        Text(account.email.isEmpty ? account.id : account.email).lineLimit(1).truncationMode(.middle)
                        Text(ProviderStyle(account.provider).name + (account.plan.isEmpty ? "" : " · " + account.plan)).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
                    }
                }
            }.width(min: 190, ideal: 240)
            TableColumn(L("状态", "Status")) { account in
                let badge = runtime.accountBadge(account)
                QuotaStateBadge(text: badge.0, color: badge.1).help(account.quota_error.map(workbenchReason) ?? localizedState(account.state))
            }.width(min: 64, ideal: 76)
            TableColumn(L("5 小时", "5 Hours")) { account in windowCell(AccountQuotaLayout(account).worstShort) }.width(min: 96, ideal: 120)
            TableColumn(L("每周", "Weekly")) { account in windowCell(AccountQuotaLayout(account).worstWeekly) }.width(min: 96, ideal: 120)
            TableColumn(L("其他限制", "Other Limits")) { account in Text(otherLimits(account)).font(.caption).foregroundStyle(.secondary).lineLimit(2).help(otherLimits(account)) }.width(min: 110, ideal: 170)
            TableColumn(L("会话", "Sessions")) { account in
                let tally = SessionTally(runtime.sessions(boundTo: account.id))
                let countStr = tally.total == 0 ? "—" : L("\(tally.total) 个", "\(tally.total)")
                Text(countStr).help(tally.detail).foregroundStyle(tally.attention > 0 ? .orange : .primary)
            }.width(min: 46, ideal: 60)
            TableColumn(L("查询", "Checked")) { account in Text(trayAgo(trayDate(account.checked_at), now: runtime.now)).font(.caption).foregroundStyle(runtime.isFresh(account) ? .secondary : Color.orange) }.width(min: 62, ideal: 80)
        }
        .contextMenu(forSelectionType: String.self) { ids in
            if let id = ids.first { Button(L("查看详情", "View Details")) { open(id) } }
        } primaryAction: { ids in
            if let id = ids.first { open(id) }
        }
    }
    // A window the source does not report is a plain dash, not an empty bar.
    @ViewBuilder private func windowCell(_ window: TrayWindow?) -> some View {
        if let window { QuotaBar(window: window, placeholder: "", now: runtime.now, compact: true) }
        else { Text("—").foregroundStyle(.tertiary).help(L("来源未提供此窗口", "Source does not provide this window")) }
    }
    private func otherLimits(_ account: TrayAccount) -> String {
        let layout = AccountQuotaLayout(account)
        let titled = layout.sections.count > 1 ? layout.sections.compactMap(\.title) : []
        let shared = titled + layout.sections.flatMap(\.others).map { w in quotaWindowTitle(w) + (w.used_percent.map { " \(Int($0.rounded()))%" } ?? "") }
        let scoped = layout.scopedGroups.map { g in g.name + (g.state == "limited" ? L(" 已满", " Full") : "") }
        let all = shared + scoped
        return all.isEmpty ? "—" : all.joined(separator: ", ")
    }
}
