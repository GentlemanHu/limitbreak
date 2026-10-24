import SwiftUI

// Why a session whose quota is back has not been continued yet. A plain quota
// wait is .none; the others name what the daemon is waiting on instead.
enum RecoveryBlock: Equatable { case none, concurrency, desktop, nativeGrace }

// When the daemon will next look at this session's quota, and when the source
// says it should come back. Both are hints: only a fresh check authorizes a send.
struct SessionClock {
    // The pool's next quota poll; never the job's deadline, which may lie in the past.
    let nextCheck: Date?
    // The job's next evaluation (not_before).
    let nextAttempt: Date?
    let expected: Date?
    let available: Int
    let total: Int
    // The expected time comes from a recorded block because the quota query is throttled.
    var fromRecordedReset = false
    var block: RecoveryBlock = .none
    var blockText = ""
}

extension TrayRuntime {
    func clock(for session: TraySession) -> SessionClock {
        clock(for: session, job: job(for: session).flatMap { $0.kind == "test" ? nil : $0 })
    }
    func clock(for session: TraySession, job: TrayJob?) -> SessionClock {
        let ids = session.pool?.account_ids ?? []
        let accounts = (snapshot?.accounts ?? []).filter { ids.contains($0.id) && $0.disabled != true }
        let available = accounts.filter { isFresh($0) && $0.state == "allowed" }.count
        // A poll a few seconds overdue is in flight; an older one only means the
        // source is polled on its own cadence, so it is not shown as "querying".
        let next = accounts.compactMap { trayDate($0.next_check_at) }.filter { $0.timeIntervalSince(now) > -20 }.min()
        // Any one account in the pool coming back is enough; within an account every applicable limit must clear.
        let hints = accounts.filter { $0.state == "limited" }.compactMap { account -> Date? in
            let groups = (account.groups ?? []).filter { $0.state == "limited" && $0.scope != "operation" }.compactMap { trayDate($0.available_at) }
            return ([trayDate(account.available_at)].compactMap { $0 } + groups).max()
        }
        // A throttled query (HTTP 429/5xx) leaves the account unknown; the daemon then
        // tries once after the recorded block's reset plus the safety margin.
        let safety = snapshot?.policy["safety_seconds"]?.number ?? 0
        let fallbackOn = snapshot?.policy["reset_fallback"]?.string != "off"
        let recorded = !fallbackOn ? [] : accounts.filter { $0.state != "allowed" && throttledQuery($0.quota_error) }.compactMap { account -> Date? in
            let blocked = (account.windows ?? []).filter { $0.blocked || ($0.used_percent ?? 0) >= 100 }
            let resets = blocked.map { trayDate($0.reset_at) }
            guard !resets.isEmpty, !resets.contains(where: { $0 == nil }) else { return nil }
            return resets.compactMap { $0 }.max().map { $0.addingTimeInterval(safety) }
        }
        let confirmed = (hints + [job.flatMap { trayDate($0.reset_hint) }].compactMap { $0 }).min()
        let fallback = recorded.min()
        let expected = available > 0 ? nil : [confirmed, fallback].compactMap { $0 }.min()
        var clock = SessionClock(nextCheck: next, nextAttempt: job.flatMap { trayDate($0.not_before) }, expected: expected, available: available, total: accounts.count,
                                 fromRecordedReset: expected != nil && expected == fallback && fallback != confirmed)
        if let job, job.active { (clock.block, clock.blockText) = recoveryBlock(session, job) }
        return clock
    }
    private func recoveryBlock(_ session: TraySession, _ job: TrayJob) -> (RecoveryBlock, String) {
        let reason = job.reason ?? ""
        let text = LanguageManager.shared.explain(reason: reason, reasonText: job.reason_text)
        // The global budget note is written on the session; the per-account one on the job.
        if session.reason.contains("并发已满") || reason.hasPrefix("account concurrency limit") {
            return (.concurrency, text)
        }
        if reason.hasPrefix("waiting for desktop:") { return (.desktop, text) }
        if reason == "等待原生自动继续（宽限期）" || reason.hasPrefix("native quota auto-resume") {
            return (.nativeGrace, L("让 Claude 自带的自动继续先行，宽限期过后再由 LimitBreak 继续，避免发两次", "Allowing Claude's native auto-continue to fire first; LimitBreak will take over after grace period"))
        }
        return (.none, "")
    }
    private func throttledQuery(_ error: String?) -> Bool {
        let text = (error ?? "").lowercased()
        if text.isEmpty || text.contains("http 401") || text.contains("http 403") { return false }
        return text.contains("http 429") || text.contains("http 5") || text.contains("timeout") || text.contains("deadline")
    }
}

// Four live tiles: what is happening now, the next quota check, the expected
// continue time and how much of the pool is usable.
struct SessionClockStrip: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    let session: TraySession
    var body: some View {
        let clock = runtime.clock(for: session)
        HStack(spacing: 8) {
            tile(L("当前", "Current"), value: current.0, detail: clock.block == .none ? current.1 : clock.blockText, icon: clock.block == .none ? current.2 : "pause.circle.fill",
                 color: clock.block == .none ? current.3 : .orange, busy: current.4)
            tile(L("下次查询额度", "Next Quota Check"), value: countdown(clock.nextCheck, due: L("查询中…", "Checking…")), detail: clockTime(clock.nextCheck) ?? L("按来源节奏查询", "Querying on source cadence"),
                 icon: "arrow.triangle.2.circlepath", color: .teal, busy: clock.nextCheck.map { $0 <= runtime.now } ?? false)
            tile(expectedTitle, value: expectedValue(clock), detail: expectedDetail(clock), icon: "clock.badge.checkmark", color: .blue, busy: false)
            tile(L("额度池", "Quota Pool"), value: clock.total == 0 ? L("未匹配", "Unmatched") : "\(clock.available)/\(clock.total) " + L("可用", "Available"), detail: clock.total == 0 ? L("可手动指定账号", "Manual account assignment available") : (clock.available > 0 ? L("撞上限额时可立即排队", "Ready to queue on rate limit") : L("全部受限或待复查", "All accounts limited or pending check")),
                 icon: "person.2.circle", color: clock.available > 0 ? .green : (clock.total == 0 ? .orange : .red), busy: false)
        }
    }
    private var hosted: Bool { ["hosted", "legacy"].contains(session.host?.mode ?? "") }
    private var expectedTitle: String { hosted && session.host?.ready != false ? L("预计自动继续", "Estimated Continue") : L("预计额度恢复", "Estimated Recovery") }
    private func expectedValue(_ clock: SessionClock) -> String {
        switch clock.block {
        case .concurrency: return L("等并发名额", "Waiting for slot")
        case .desktop: return L("等桌面就绪", "Waiting for desktop")
        case .nativeGrace: return countdown(clock.nextAttempt, due: L("即将接手", "Taking over soon"))
        case .none: break
        }
        if clock.available > 0 { return session.state == "COOLDOWN" || session.state == "READY_TO_RESUME" ? L("复查确认后", "After check confirmation") : L("无需等待", "No wait needed") }
        return countdown(clock.expected, due: clock.fromRecordedReset ? L("即将尝试", "Retrying soon") : L("复查确认中", "Confirming check"))
    }
    private func expectedDetail(_ clock: SessionClock) -> String {
        if clock.block != .none { return L("额度已可用，", "Quota is available; ") + (clock.block == .nativeGrace ? L("宽限期后由 LimitBreak 接手", "LimitBreak takes over after grace period") : L("条件满足后立即发送", "sending as soon as conditions are met")) }
        if clock.available > 0 { return L("池中有账号可用", "Account available in pool") }
        guard let at = clock.expected else { return clock.total == 0 ? L("没有可等待的账号", "No accounts to wait on") : L("来源未给出重置时间", "Source provided no reset time") }
        let when = at.formatted(Date.FormatStyle().weekday(.abbreviated).hour().minute().locale(lang.locale))
        return clock.fromRecordedReset ? when + L(" · 查询被限流，按记录的重置时间尝试", " · Throttled, retrying at recorded reset") : when + L(" · 以复查为准", " · Subject to check")
    }
    private var current: (String, String, String, Color, Bool) {
        switch session.state {
        case "RUNNING": return (L("运行中", "Running"), hosted ? L("撞上限额后自动排队", "Queues automatically when rate-limited") : L("撞上限额后只记录", "Logs only when rate-limited"), "play.circle.fill", .green, false)
        case "COOLDOWN": return (L("等待恢复", "Waiting"), L("到点复查，确认后发送", "Checks at reset time; sends on confirmation"), "hourglass", .indigo, false)
        case "READY_TO_RESUME": return (L("额度已恢复", "Quota Restored"), L("排队等待发送", "Queued waiting to send"), "checkmark.circle.fill", .blue, false)
        case "DISPATCHING": return (L("正在发送", "Sending"), L("打开会话并写入继续文案", "Opening session and writing continue prompt"), "paperplane.fill", .blue, true)
        case "VERIFYING": return (L("等待回执", "Awaiting Receipt"), L("确认会话收到消息", "Verifying session received message"), "checkmark.message", .blue, true)
        case "NEEDS_ATTENTION": return (L("需要处理", "Needs Attention"), L("不会自动重发", "Will not resend automatically"), "exclamationmark.triangle.fill", .orange, false)
        case "COMPLETED": return (L("已继续", "Continued"), L("本次已完成", "Completed this round"), "checkmark.seal.fill", .green, false)
        default: return (localizedState(session.state.isEmpty ? "unknown" : session.state), hosted ? L("已托管", "Hosted") : TrayHostMode.localizedTitle(session.host?.mode ?? ""), "circle.dashed", .secondary, false)
        }
    }
    private func countdown(_ date: Date?, due: String) -> String {
        guard let date else { return "—" }
        return date > runtime.now ? localizedDuration(date.timeIntervalSince(runtime.now)) : due
    }
    private func clockTime(_ date: Date?) -> String? { date.map { $0.formatted(Date.FormatStyle(date: .omitted, time: .standard, locale: lang.locale)) } }
    private func tile(_ title: String, value: String, detail: String, icon: String, color: Color, busy: Bool) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 5) {
                if busy { ProgressView().controlSize(.mini) } else { Image(systemName: icon).foregroundStyle(color) }
                Text(title).font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Text(value).font(.title3.weight(.semibold).monospacedDigit()).lineLimit(1).minimumScaleFactor(0.7)
            Text(detail).font(.caption2).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
        }.padding(10).frame(maxWidth: .infinity, minHeight: 84, alignment: .topLeading)
            .background(color.opacity(0.07), in: RoundedRectangle(cornerRadius: 9))
            .overlay(RoundedRectangle(cornerRadius: 9).strokeBorder(color.opacity(0.18)))
    }
}
