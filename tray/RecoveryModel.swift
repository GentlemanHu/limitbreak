import SwiftUI

// One recovery: a resume job from the moment the session hit its limit until it
// was continued, stopped or handed to the user. Everything shown derives from the
// job, its session and the pool accounts in the latest snapshot.
struct RecoveryTask: Identifiable {
    let job: TrayJob
    let session: TraySession?
    let clock: SessionClock?
    var id: String { job.stableID }
    var title: String { session?.displayTitle ?? String(job.session_id.prefix(8)) }
    var started: Date? { trayDate(job.created_at) }
    var finished: Date? { ["COMPLETED", "CANCELLED"].contains(job.state) ? trayDate(job.updated_at) : nil }
    var hosted: Bool { ["hosted", "legacy"].contains(session?.host?.mode ?? "") }
    var pipeline: RecoveryPipeline { RecoveryPipeline(job: job, clock: clock) }
    // Lower sorts first: what needs the user, what is moving, then the soonest continue.
    var urgency: Int {
        switch job.state {
        case "NEEDS_ATTENTION": return 0
        case "DISPATCHING", "VERIFYING": return 1
        case "READY_TO_RESUME": return 2
        case "COOLDOWN": return clock?.block == RecoveryBlock.none ? 4 : 3
        default: return 9
        }
    }
}

enum RecoveryFilter: String, CaseIterable, Identifiable {
    case active, attention, finished, all
    var id: String { rawValue }
    var title: String {
        switch self {
        case .active: return "进行中"
        case .attention: return "需处理"
        case .finished: return "近 24 小时已结束"
        case .all: return "全部"
        }
    }
}

// The six steps of a recovery. `current` is the step in progress; earlier steps are done.
struct RecoveryPipeline {
    enum Status { case done, active, waiting, failed, cancelled, pending }
    static let titles = ["撞上限额", "等待额度恢复", "复查确认额度", "发送继续", "会话回执", "已继续"]
    static let icons = ["exclamationmark.octagon", "hourglass", "arrow.triangle.2.circlepath", "paperplane", "checkmark.message", "checkmark.seal"]
    let current: Int
    let failed: Bool
    let cancelled: Bool
    let blocked: Bool
    init(job: TrayJob, clock: SessionClock?) {
        let block = clock?.block ?? .none
        failed = job.state == "NEEDS_ATTENTION"
        cancelled = job.state == "CANCELLED"
        blocked = block != .none
        switch job.state {
        case "COOLDOWN": current = block != .none ? 3 : ((clock?.available ?? 0) > 0 ? 2 : 1)
        case "READY_TO_RESUME", "DISPATCHING": current = 3
        case "VERIFYING": current = 4
        case "COMPLETED": current = Self.titles.count
        case "NEEDS_ATTENTION": current = (job.attempts ?? 0) > 0 ? 4 : 3
        default: current = 1
        }
    }
    func status(_ step: Int) -> Status {
        if step < current { return .done }
        if step > current { return .pending }
        if failed { return .failed }
        if cancelled { return .cancelled }
        return blocked ? .waiting : .active
    }
}

struct RecoverySummary {
    var waiting = 0, sending = 0, attention = 0, finished = 0
    var nearest: (task: RecoveryTask, at: Date)?
}

extension TrayRuntime {
    // Active jobs always; finished ones for a day so a continue that just happened stays visible.
    func recoveryTasks(_ filter: RecoveryFilter) -> [RecoveryTask] {
        let horizon = now.addingTimeInterval(-86400)
        let tasks = index.recoveryJobs.compactMap { job -> RecoveryTask? in
            let recent = (trayDate(job.updated_at) ?? .distantPast) >= horizon
            switch filter {
            case .active: guard job.active, job.state != "NEEDS_ATTENTION" else { return nil }
            case .attention: guard job.state == "NEEDS_ATTENTION" else { return nil }
            case .finished: guard !job.active, recent else { return nil }
            case .all: guard job.active || recent else { return nil }
            }
            let session = index.sessionByID[job.session_id]
            return RecoveryTask(job: job, session: session, clock: session.map { clock(for: $0, job: job) })
        }
        return tasks.sorted {
            if $0.urgency != $1.urgency { return $0.urgency < $1.urgency }
            if $0.job.active { return ($0.clock?.expected ?? .distantFuture) < ($1.clock?.expected ?? .distantFuture) }
            return ($0.finished ?? .distantPast) > ($1.finished ?? .distantPast)
        }
    }
    var recoverySummary: RecoverySummary {
        var summary = RecoverySummary()
        for task in recoveryTasks(.all) {
            switch task.job.state {
            case "COOLDOWN", "READY_TO_RESUME": summary.waiting += 1
            case "DISPATCHING", "VERIFYING": summary.sending += 1
            case "NEEDS_ATTENTION": summary.attention += 1
            case "COMPLETED": summary.finished += 1
            default: break
            }
            if let at = task.clock?.expected, at > now, task.job.active, summary.nearest.map({ at < $0.at }) ?? true { summary.nearest = (task, at) }
        }
        return summary
    }
    func poolAccounts(_ session: TraySession?) -> [TrayAccount] {
        let ids = session?.pool?.account_ids ?? []
        return (snapshot?.accounts ?? []).filter { ids.contains($0.id) }
    }
    // When a limited account comes back: its own reset, or the latest of its limiting groups.
    func accountReset(_ account: TrayAccount) -> Date? {
        guard account.state == "limited" else { return nil }
        let groups = (account.groups ?? []).filter { $0.state == "limited" && $0.scope != "operation" }.compactMap { trayDate($0.available_at) }
        return ([trayDate(account.available_at)].compactMap { $0 } + groups).max()
    }
}

// "02:19:08" today, "周二 02:19" otherwise.
func recoveryTime(_ date: Date?, now: Date) -> String {
    guard let date else { return "—" }
    if Calendar.current.isDate(date, inSameDayAs: now) { return date.formatted(date: .omitted, time: .standard) }
    return date.formatted(.dateTime.weekday(.abbreviated).hour().minute())
}
