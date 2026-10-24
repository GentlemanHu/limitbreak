import SwiftUI

// A grid card: who, how much of the 5-hour and weekly windows is used,
// scoped extra limits, and which sessions depend on the account.
struct QuotaTile: View {
    @ObservedObject var runtime: TrayRuntime
    let account: TrayAccount
    let open: () -> Void
    var body: some View {
        let layout = AccountQuotaLayout(account)
        let badge = runtime.accountBadge(account)
        let tally = SessionTally(runtime.sessions(boundTo: account.id))
        Button(action: open) {
            VStack(alignment: .leading, spacing: 12) {
                HStack(spacing: 10) {
                    ProviderIcon(provider: account.provider, size: 32)
                    VStack(alignment: .leading, spacing: 2) {
                        Text(account.email.isEmpty ? account.id : account.email).font(.callout.weight(.semibold)).lineLimit(1).truncationMode(.middle)
                        Text(ProviderStyle(account.provider).name + (account.plan.isEmpty ? "" : " · " + account.plan)).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                    }
                    Spacer(minLength: 4)
                    QuotaStateBadge(text: badge.0, color: badge.1)
                }
                if account.disabled == true {
                    Text(L("该凭证已在来源中停用，不参与额度判断。", "Credential disabled in source; excluded from quota evaluation.")).font(.caption).foregroundStyle(.secondary)
                } else {
                    if layout.isEmpty {
                        Text(account.reason.isEmpty ? L("来源没有提供额度窗口", "No quota windows provided by source") : workbenchReason(account.reason)).font(.caption).foregroundStyle(.secondary)
                    }
                    ForEach(Array(layout.sections.enumerated()), id: \.offset) { _, section in
                        if !section.isEmpty { QuotaSectionBars(section: section, now: runtime.now) }
                    }
                    if !layout.scopedGroups.isEmpty { QuotaGroupChips(runtime: runtime, groups: layout.scopedGroups) }
                }
                if let error = account.quota_error, !error.isEmpty {
                    Label(workbenchReason(error), systemImage: "exclamationmark.triangle").font(.caption2).foregroundStyle(.orange).lineLimit(2)
                }
                Divider()
                HStack(spacing: 6) {
                    Image(systemName: "bubble.left.and.bubble.right").foregroundStyle(tally.total > 0 ? Color.accentColor : .secondary)
                    Text(tally.total > 0 ? L("\(tally.total) 个会话", "\(tally.total) session(s)") : L("未绑定会话", "No bound sessions")).fontWeight(tally.total > 0 ? .medium : .regular)
                    if tally.total > 0 { Text(tally.detail).foregroundStyle(.secondary).lineLimit(1) }
                    Spacer(minLength: 4)
                    Text(checkedText).foregroundStyle(.secondary).lineLimit(1)
                }.font(.caption2)
            }
            .padding(14).frame(maxWidth: .infinity, alignment: .leading)
            .background(.background, in: RoundedRectangle(cornerRadius: 14))
            .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(badge.1.opacity(account.state == "limited" ? 0.45 : 0.12), lineWidth: 1))
            .contentShape(RoundedRectangle(cornerRadius: 14))
        }.buttonStyle(.plain).accessibilityLabel("\(account.email) \(badge.0)")
    }
    private var checkedText: String {
        let checked = L("查询于 ", "Checked ") + trayAgo(trayDate(account.checked_at), now: runtime.now)
        guard let next = trayDate(account.next_check_at), next > runtime.now else { return checked }
        return checked + L(" · " + localizedDuration(next.timeIntervalSince(runtime.now)) + " 后复查", " · check in " + localizedDuration(next.timeIntervalSince(runtime.now)))
    }
}

// A missing 5-hour or weekly slot is shown as missing only when its partner exists.
struct QuotaSectionBars: View {
    let section: QuotaSection
    let now: Date
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let title = section.title {
                Label(title, systemImage: "square.stack.3d.up").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    .help(L("适用范围未知：保守地视为影响这个账号的所有会话", "Unknown scope: treated as account-wide"))
            }
            if section.short != nil || section.weekly != nil {
                QuotaBar(window: section.short, placeholder: L("5 小时", "5 Hours"), now: now)
                QuotaBar(window: section.weekly, placeholder: L("每周", "Weekly"), now: now)
            }
            ForEach(Array(section.others.prefix(2).enumerated()), id: \.offset) { _, w in QuotaBar(window: w, placeholder: "", now: now, compact: true) }
        }
    }
}

// Model-family, operation and unknown-scope limits, each with what it actually restricts.
struct QuotaGroupChips: View {
    @ObservedObject var runtime: TrayRuntime
    let groups: [TrayQuotaGroup]
    var body: some View {
        FlowChips(items: groups.map { group in
            let window = (group.windows ?? []).max { ($0.used_percent ?? 0) < ($1.used_percent ?? 0) }
            let fresh = runtime.isFresh(group)
            let limited = group.state == "limited"
            let color: Color = !fresh ? .gray : limited ? .red : quotaUsageColor(window?.used_percent, blocked: false)
            let value = window?.used_percent.map { "\(Int($0.rounded()))%" } ?? (limited ? L("已满", "Full") : "")
            let icon = group.scope == "models" ? "cpu" : group.scope == "operation" ? "text.magnifyingglass" : "questionmark.circle"
            var help = runtime.groupScopeText(group)
            if let reset = trayDate(group.available_at) ?? trayDate(window?.reset_at) {
                help += "\n" + L("预计重置：", "Estimated reset: ") + reset.formatted(date: .abbreviated, time: .shortened)
            }
            return FlowChip(id: group.id, text: group.name + (value.isEmpty ? "" : " " + value), icon: icon, color: color, help: help)
        })
    }
}

struct FlowChip: Identifiable {
    let id: String
    let text: String
    let icon: String
    let color: Color
    let help: String
}

// Wraps chips onto as many lines as needed.
struct FlowChips: View {
    let items: [FlowChip]
    var body: some View {
        WrapLayout(spacing: 6) {
            ForEach(items) { chip in
                Label(chip.text, systemImage: chip.icon).font(.caption2.weight(.medium)).foregroundStyle(chip.color).lineLimit(1)
                    .padding(.horizontal, 7).padding(.vertical, 3).background(chip.color.opacity(0.11), in: Capsule()).help(chip.help)
            }
        }
    }
}

struct WrapLayout: Layout {
    var spacing: CGFloat = 6
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? .infinity
        var x: CGFloat = 0, y: CGFloat = 0, line: CGFloat = 0, widest: CGFloat = 0
        for view in subviews {
            let size = view.sizeThatFits(.unspecified)
            if x > 0 && x + size.width > width { y += line + spacing; x = 0; line = 0 }
            x += size.width + spacing
            line = max(line, size.height)
            widest = max(widest, x - spacing)
        }
        return CGSize(width: min(widest, width), height: y + line)
    }
    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var x = bounds.minX, y = bounds.minY, line: CGFloat = 0
        for view in subviews {
            let size = view.sizeThatFits(.unspecified)
            if x > bounds.minX && x + size.width > bounds.maxX { y += line + spacing; x = bounds.minX; line = 0 }
            view.place(at: CGPoint(x: x, y: y), proposal: ProposedViewSize(size))
            x += size.width + spacing
            line = max(line, size.height)
        }
    }
}

// Cards of different heights packed into the shortest column, so short cards
// do not leave a hole beside a tall neighbour.
struct MasonryLayout: Layout {
    var minColumnWidth: CGFloat = 300
    var spacing: CGFloat = 14
    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? minColumnWidth
        return CGSize(width: width, height: arrange(width: width, subviews: subviews).height)
    }
    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        for (view, frame) in zip(subviews, arrange(width: bounds.width, subviews: subviews).frames) {
            view.place(at: CGPoint(x: bounds.minX + frame.minX, y: bounds.minY + frame.minY), proposal: ProposedViewSize(frame.size))
        }
    }
    private func arrange(width: CGFloat, subviews: Subviews) -> (frames: [CGRect], height: CGFloat) {
        let columns = max(1, Int((width + spacing) / (minColumnWidth + spacing)))
        let columnWidth = max(0, (width - spacing * CGFloat(columns - 1)) / CGFloat(columns))
        var heights = [CGFloat](repeating: 0, count: columns)
        var frames: [CGRect] = []
        for view in subviews {
            let column = heights.indices.min { heights[$0] < heights[$1] } ?? 0
            let height = view.sizeThatFits(ProposedViewSize(width: columnWidth, height: nil)).height
            frames.append(CGRect(x: CGFloat(column) * (columnWidth + spacing), y: heights[column], width: columnWidth, height: height))
            heights[column] += height + spacing
        }
        return (frames, max(0, (heights.max() ?? 0) - spacing))
    }
}
