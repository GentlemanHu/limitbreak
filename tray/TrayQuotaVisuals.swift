import AppKit
import SwiftUI

// Shared visual vocabulary for quota: provider identity, window titles, usage colours.
struct ProviderStyle {
    let name: String
    let icon: String
    let color: Color
    // Brand mark in Resources/ProviderIcons; "<asset>-dark.png" exists for monochrome marks.
    let asset: String?
    init(_ provider: String) {
        let p = provider.lowercased()
        let known: [(keys: [String], name: String, icon: String, color: Color, asset: String)] = [
            (["claude", "anthropic"], "Claude", "sparkle", Color(red: 0.85, green: 0.47, blue: 0.34), "claude"),
            (["codex"], "Codex", "terminal.fill", Color(red: 0.06, green: 0.62, blue: 0.48), "codex"),
            (["openai"], "OpenAI", "terminal.fill", Color(red: 0.06, green: 0.62, blue: 0.48), "openai"),
            (["antigravity"], "Antigravity", "atom", .purple, "antigravity"),
            (["vertex"], "Vertex AI", "sparkles", .blue, "vertex"),
            (["aistudio", "ai-studio"], "AI Studio", "sparkles", .blue, "aistudio"),
            (["gemini"], "Gemini", "sparkles", .blue, "gemini"),
            (["qwen"], "Qwen", "cloud.fill", .indigo, "qwen"),
            (["kimi", "moonshot"], "Kimi", "moon.stars.fill", .teal, "kimi"),
            (["xai", "grok"], "xAI", "xmark", Color(white: 0.32), "xai"),
            (["iflow"], "iFlow", "wind", .cyan, "iflow"),
            (["deepseek"], "DeepSeek", "water.waves", .blue, "deepseek"),
            (["openrouter"], "OpenRouter", "arrow.triangle.branch", .indigo, "openrouter"),
        ]
        let match = known.first { $0.keys.contains { key in p.contains(key) } }
        name = match?.name ?? (provider.isEmpty ? "未知供应商" : provider)
        icon = match?.icon ?? "server.rack"
        color = match?.color ?? .gray
        asset = match?.asset
    }
}

@MainActor
private enum ProviderArtwork {
    private static var cache: [String: NSImage] = [:]
    private static var missing = Set<String>()
    static func image(_ name: String) -> NSImage? {
        if let hit = cache[name] { return hit }
        if missing.contains(name) { return nil }
        guard let url = Bundle.main.url(forResource: name, withExtension: "png", subdirectory: "ProviderIcons"), let image = NSImage(contentsOf: url) else {
            missing.insert(name); return nil
        }
        cache[name] = image
        return image
    }
}

struct ProviderIcon: View {
    let provider: String
    var size: CGFloat = 30
    @Environment(\.colorScheme) private var scheme
    var body: some View {
        let style = ProviderStyle(provider)
        Group {
            if let artwork {
                Image(nsImage: artwork).resizable().interpolation(.high).scaledToFit().padding(size * 0.14)
                    .frame(width: size, height: size)
                    .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: size * 0.28))
                    .overlay(RoundedRectangle(cornerRadius: size * 0.28).strokeBorder(.separator.opacity(0.6), lineWidth: 0.5))
            } else {
                Image(systemName: style.icon).font(.system(size: size * 0.48, weight: .semibold)).foregroundStyle(.white)
                    .frame(width: size, height: size).background(style.color.gradient, in: RoundedRectangle(cornerRadius: size * 0.28))
            }
        }.help(style.name)
    }
    @MainActor private var artwork: NSImage? {
        guard let asset = ProviderStyle(provider).asset else { return nil }
        if scheme == .dark, let dark = ProviderArtwork.image(asset + "-dark") { return dark }
        return ProviderArtwork.image(asset)
    }
}

enum QuotaWindowKind { case short, weekly, other }

func quotaWindowKind(_ w: TrayWindow) -> QuotaWindowKind {
    if let s = w.limit_seconds, s > 0 { return s <= 86400 ? .short : (s >= 6 * 86400 && s <= 8 * 86400 ? .weekly : .other) }
    let n = w.name.lowercased()
    if n.contains("5 小时") || n.contains("five") || n.contains("primary") { return .short }
    if n.contains("7 天") || n.contains("周") || n.contains("week") || n.contains("secondary") || n.contains("seven") { return .weekly }
    return .other
}

func quotaWindowTitle(_ w: TrayWindow) -> String {
    if let s = w.limit_seconds, s > 0 {
        switch s {
        case 18000: return L("5 小时", "5 Hours")
        case 86400: return L("每日", "Daily")
        case 604800: return L("每周", "Weekly")
        case 2_592_000: return L("每月", "Monthly")
        default: return s % 3600 == 0 ? L("\(s / 3600) 小时", "\(s / 3600)h") : L("\(s / 60) 分钟", "\(s / 60)m")
        }
    }
    if w.name.unicodeScalars.contains(where: { $0.value >= 0x4E00 && $0.value <= 0x9FFF }) { return w.name }
    if w.name == "code review quota" { return L("代码审查", "Code Review") }
    if w.name == "credential status" { return L("凭证状态", "Credential Status") }
    return w.name.replacingOccurrences(of: "rate_limit:", with: "").replacingOccurrences(of: "_window", with: "").replacingOccurrences(of: "_", with: " ")
}

// One set of bars: a 5-hour slot, a weekly slot and anything else.
struct QuotaSection {
    let title: String?
    let short: TrayWindow?
    let weekly: TrayWindow?
    let others: [TrayWindow]
    init(title: String?, windows: [TrayWindow]) {
        self.title = title
        var short: TrayWindow?, weekly: TrayWindow?, others: [TrayWindow] = []
        for w in windows {
            switch quotaWindowKind(w) {
            case .short where short == nil: short = w
            case .weekly where weekly == nil: weekly = w
            default: others.append(w)
            }
        }
        self.short = short
        self.weekly = weekly
        self.others = others
    }
    var isEmpty: Bool { short == nil && weekly == nil && others.isEmpty }
}

// Limits that stop every conversation are drawn as bars; limits that only stop
// some models or one operation are chips. A group of unknown scope is
// conservatively treated as account-wide: a bar when nothing is declared
// account-wide, a chip next to declared account-wide windows otherwise.
struct AccountQuotaLayout {
    let sections: [QuotaSection]
    let scopedGroups: [TrayQuotaGroup]
    init(_ account: TrayAccount) {
        let groups = account.groups ?? []
        let shared = groups.filter { $0.scope == "account" }
        let unknown = groups.filter { !["account", "models", "operation"].contains($0.scope) && !($0.windows ?? []).isEmpty }
        if !shared.isEmpty {
            sections = [QuotaSection(title: nil, windows: shared.flatMap { $0.windows ?? [] })]
            scopedGroups = groups.filter { $0.scope != "account" }
        } else if unknown.count > 1 {
            sections = unknown.map { QuotaSection(title: $0.name, windows: $0.windows ?? []) }
            scopedGroups = groups.filter { ["models", "operation"].contains($0.scope) }
        } else if let only = unknown.first {
            sections = [QuotaSection(title: nil, windows: only.windows ?? [])]
            scopedGroups = groups.filter { ["models", "operation"].contains($0.scope) }
        } else {
            sections = [QuotaSection(title: nil, windows: (account.windows ?? []).filter { !$0.name.hasPrefix("model:") })]
            scopedGroups = groups.filter { $0.scope != "account" }
        }
    }
    var short: TrayWindow? { sections.first?.short }
    var weekly: TrayWindow? { sections.first?.weekly }
    var isEmpty: Bool { sections.allSatisfy(\.isEmpty) }
    // Compact views show the most-used window of each kind across sections.
    var worstShort: TrayWindow? { Self.worst(sections.compactMap(\.short)) }
    var worstWeekly: TrayWindow? { Self.worst(sections.compactMap(\.weekly)) }
    private static func worst(_ windows: [TrayWindow]) -> TrayWindow? {
        windows.max { a, b in (a.blocked ? 101 : a.used_percent ?? -1) < (b.blocked ? 101 : b.used_percent ?? -1) }
    }
}

func quotaUsageColor(_ used: Double?, blocked: Bool) -> Color {
    if blocked { return .red }
    guard let used else { return .gray }
    return used >= 90 ? .red : used >= 70 ? .orange : .green
}

func trayAgo(_ date: Date?, now: Date) -> String {
    guard let date else { return L("未知", "Unknown") }
    let s = Int(now.timeIntervalSince(date))
    if s < 0 { return date.formatted(date: .omitted, time: .shortened) }
    if s < 60 { return L("刚刚", "Just now") }
    if s < 3600 { return L("\(s / 60) 分钟前", "\(s / 60)m ago") }
    if s < 86400 { return L("\(s / 3600) 小时前", "\(s / 3600)h ago") }
    if s < 7 * 86400 { return L("\(s / 86400) 天前", "\(s / 86400)d ago") }
    return date.formatted(.dateTime.month(.abbreviated).day())
}

// One quota window: title, percentage, bar and reset countdown.
struct QuotaBar: View {
    let window: TrayWindow?
    let placeholder: String
    let now: Date
    var compact = false
    var body: some View {
        VStack(alignment: .leading, spacing: compact ? 2 : 4) {
            HStack(spacing: 6) {
                Text(window.map(quotaWindowTitle) ?? placeholder).font(compact ? .caption2 : .caption.weight(.medium)).foregroundStyle(compact ? .secondary : .primary).lineLimit(1)
                Spacer(minLength: 4)
                Text(percent).font((compact ? Font.caption2 : .caption).monospacedDigit().weight(.semibold)).foregroundStyle(color)
            }
            ProgressView(value: fraction).tint(color).controlSize(compact ? .mini : .small)
            if !compact, let note { Text(note).font(.caption2).foregroundStyle(.secondary).lineLimit(1) }
        }.help(help)
    }
    private var used: Double? {
        guard let u = window?.used_percent, u.isFinite else { return nil }
        return min(100, max(0, u))
    }
    private var color: Color { window == nil ? .gray : quotaUsageColor(used, blocked: window?.blocked == true) }
    private var fraction: Double { (used ?? (window?.blocked == true ? 100 : 0)) / 100 }
    private var percent: String {
        guard let window else { return "—" }
        if let used { return "\(Int(used.rounded()))%" }
        return window.blocked ? L("已满", "Full") : L("未知", "Unknown")
    }
    private var reset: Date? { trayDate(window?.reset_at) }
    private var note: String? {
        guard let window else { return L("来源未提供此窗口", "Source does not provide this window") }
        if let reset {
            if reset <= now { return L("已到重置时间，等待复查确认", "Reset time reached; waiting for check confirmation") }
            return L("\(trayDuration(reset.timeIntervalSince(now))) 后重置 · \(reset.formatted(.dateTime.weekday(.abbreviated).hour().minute()))",
                     "Resets in \(trayDuration(reset.timeIntervalSince(now))) · \(reset.formatted(.dateTime.weekday(.abbreviated).hour().minute()))")
        }
        return window.blocked ? L("已受限，来源未给出重置时间", "Limited; source provided no reset time") : nil
    }
    private var help: String {
        guard let reset else { return note ?? "" }
        return L("预计重置：" + reset.formatted(date: .complete, time: .shortened) + "（以复查到的新额度为准，也可能提前重置）",
                 "Estimated reset: " + reset.formatted(date: .complete, time: .shortened) + " (subject to live quota check; may reset early)")
    }
}

struct QuotaStateBadge: View {
    let text: String
    let color: Color
    var body: some View {
        Text(text).font(.caption2.weight(.semibold)).foregroundStyle(color)
            .padding(.horizontal, 7).padding(.vertical, 2).background(color.opacity(0.13), in: Capsule())
    }
}

extension TrayRuntime {
    func accountBadge(_ account: TrayAccount) -> (String, Color) {
        if account.disabled == true { return (L("已停用", "Disabled"), .gray) }
        if !isFresh(account) { return (account.quota_error?.isEmpty == false ? L("查询失败", "Error") : L("等待复查", "Checking"), .orange) }
        switch account.state {
        case "allowed": return (AccountQuotaLayout(account).scopedGroups.contains { $0.state == "limited" } ? L("部分受限", "Partial") : L("可用", "Active"), .green)
        case "limited": return (L("受限", "Limited"), .red)
        default: return (L("未知", "Unknown"), .gray)
        }
    }
    func groupScopeText(_ group: TrayQuotaGroup) -> String {
        switch group.scope {
        case "models": return L("只限制 " + (group.models ?? []).joined(separator: "、") + " 模型的会话", "Only limits sessions using " + (group.models ?? []).joined(separator: ", ") + " models")
        case "operation": return L("只限制代码审查等单项操作，不影响对话续写", "Only limits single operations like code review; does not block session continuation")
        case "account": return L("账号共享额度，影响所有会话", "Shared account quota; affects all sessions")
        default: return L("适用范围未知：保守地视为影响整个账号", "Unknown scope: treated as account-wide")
        }
    }
    func sessions(boundTo accountID: String) -> [TraySession] { index.sessionsByAccount[accountID] ?? [] }
}

struct SessionTally {
    var total = 0, hosted = 0, monitored = 0, waiting = 0, attention = 0, running = 0
    init(_ sessions: [TraySession]) {
        for s in sessions {
            total += 1
            if s.host?.mode == "hosted" || s.host?.mode == "legacy" { hosted += 1 } else if s.host?.mode == "monitor" { monitored += 1 }
            switch s.state {
            case "NEEDS_ATTENTION": attention += 1
            case "COOLDOWN", "READY_TO_RESUME", "VERIFYING", "DISPATCHING": waiting += 1
            case "RUNNING": running += 1
            default: break
            }
        }
    }
    var detail: String {
        guard total > 0 else { return L("未绑定会话", "No bound sessions") }
        var parts = [L("托管 \(hosted)", "Hosted \(hosted)"), L("监控 \(monitored)", "Monitored \(monitored)")]
        if waiting > 0 { parts.append(L("等待 \(waiting)", "Waiting \(waiting)")) }
        if attention > 0 { parts.append(L("需处理 \(attention)", "Needs attention \(attention)")) }
        return parts.joined(separator: " · ")
    }
}
