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
        let text = job.reason_text?.isEmpty == false ? job.reason_text! : reason
        // The global budget note is written on the session; the per-account one on the job.
        if session.reason.contains("并发已满") { return (.concurrency, session.explanation) }
        if reason.hasPrefix("account concurrency limit") { return (.concurrency, text) }
        if reason.hasPrefix("waiting for desktop:") { return (.desktop, text) }
        if reason == "等待原生自动继续（宽限期）" { return (.nativeGrace, "让 Claude 自带的自动继续先行，宽限期过后再由 LimitBreak 继续，避免发两次") }
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
    let session: TraySession
    var body: some View {
        let clock = runtime.clock(for: session)
        HStack(spacing: 8) {
            tile("当前", value: current.0, detail: clock.block == .none ? current.1 : clock.blockText, icon: clock.block == .none ? current.2 : "pause.circle.fill",
                 color: clock.block == .none ? current.3 : .orange, busy: current.4)
            tile("下次查询额度", value: countdown(clock.nextCheck, due: "查询中…"), detail: clockTime(clock.nextCheck) ?? "按来源节奏查询",
                 icon: "arrow.triangle.2.circlepath", color: .teal, busy: clock.nextCheck.map { $0 <= runtime.now } ?? false)
            tile(expectedTitle, value: expectedValue(clock), detail: expectedDetail(clock), icon: "clock.badge.checkmark", color: .blue, busy: false)
            tile("额度池", value: clock.total == 0 ? "未匹配" : "\(clock.available)/\(clock.total) 可用", detail: clock.total == 0 ? "可手动指定账号" : (clock.available > 0 ? "撞上限额时可立即排队" : "全部受限或待复查"),
                 icon: "person.2.circle", color: clock.available > 0 ? .green : (clock.total == 0 ? .orange : .red), busy: false)
        }
    }
    private var hosted: Bool { ["hosted", "legacy"].contains(session.host?.mode ?? "") }
    private var expectedTitle: String { hosted && session.host?.ready != false ? "预计自动继续" : "预计额度恢复" }
    private func expectedValue(_ clock: SessionClock) -> String {
        switch clock.block {
        case .concurrency: return "等并发名额"
        case .desktop: return "等桌面就绪"
        case .nativeGrace: return countdown(clock.nextAttempt, due: "即将接手")
        case .none: break
        }
        if clock.available > 0 { return session.state == "COOLDOWN" || session.state == "READY_TO_RESUME" ? "复查确认后" : "无需等待" }
        return countdown(clock.expected, due: clock.fromRecordedReset ? "即将尝试" : "复查确认中")
    }
    private func expectedDetail(_ clock: SessionClock) -> String {
        if clock.block != .none { return "额度已可用，" + (clock.block == .nativeGrace ? "宽限期后由 LimitBreak 接手" : "条件满足后立即发送") }
        if clock.available > 0 { return "池中有账号可用" }
        guard let at = clock.expected else { return clock.total == 0 ? "没有可等待的账号" : "来源未给出重置时间" }
        let when = at.formatted(.dateTime.weekday(.abbreviated).hour().minute())
        return clock.fromRecordedReset ? when + " · 查询被限流，按记录的重置时间尝试" : when + " · 以复查为准"
    }
    private var current: (String, String, String, Color, Bool) {
        switch session.state {
        case "RUNNING": return ("运行中", hosted ? "撞上限额后自动排队" : "撞上限额后只记录", "play.circle.fill", .green, false)
        case "COOLDOWN": return ("等待恢复", "到点复查，确认后发送", "hourglass", .indigo, false)
        case "READY_TO_RESUME": return ("额度已恢复", "排队等待发送", "checkmark.circle.fill", .blue, false)
        case "DISPATCHING": return ("正在发送", "打开会话并写入继续文案", "paperplane.fill", .blue, true)
        case "VERIFYING": return ("等待回执", "确认会话收到消息", "checkmark.message", .blue, true)
        case "NEEDS_ATTENTION": return ("需要处理", "不会自动重发", "exclamationmark.triangle.fill", .orange, false)
        case "COMPLETED": return ("已继续", "本次已完成", "checkmark.seal.fill", .green, false)
        default: return (trayState(session.state.isEmpty ? "unknown" : session.state), hosted ? "已托管" : TrayHostMode.title(session.host?.mode ?? ""), "circle.dashed", .secondary, false)
        }
    }
    private func countdown(_ date: Date?, due: String) -> String {
        guard let date else { return "—" }
        return date > runtime.now ? trayDuration(date.timeIntervalSince(runtime.now)) : due
    }
    private func clockTime(_ date: Date?) -> String? { date.map { $0.formatted(date: .omitted, time: .standard) } }
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
