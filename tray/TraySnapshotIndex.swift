import Foundation

// Derived once per received snapshot. Views re-render every second (countdowns),
// so per-row lookups must be dictionary reads, never scans or date parsing.
struct TraySnapshotIndex {
    let sessionsByActivity: [TraySession]
    let lastActive: [String: Date]
    let nextJob: [String: TrayJob]
    let latestTest: [String: TrayJob]
    let events: [String: [TrayAudit]]
    // Live sessions whose pool, binding or monitor selection includes the account.
    let sessionsByAccount: [String: [TraySession]]
    let sessionByID: [String: TraySession]
    // Every resume job (tests excluded), newest first; the recovery page filters by time.
    let recoveryJobs: [TrayJob]
    // The newest job per session that recorded how it was delivered.
    let lastDelivery: [String: TrayJob]

    static let eventLimit = 8

    init(_ snapshot: TraySnapshot?) {
        let sessions = snapshot?.sessions ?? []
        // Claude's own last-activity time when known, else the guard's last change.
        var active: [String: Date] = [:]
        for session in sessions {
            if let at = [trayDate(session.last_activity_at), trayDate(session.updated_at)].compactMap({ $0 }).max() { active[session.id] = at }
        }
        lastActive = active
        sessionsByActivity = sessions.sorted { (active[$0.id] ?? .distantPast) > (active[$1.id] ?? .distantPast) }
        var byAccount: [String: [TraySession]] = [:]
        for session in sessionsByActivity where !session.isArchived {
            var ids = Set<String>(session.pool?.account_ids ?? [])
            ids.formUnion(session.binding?.account_ids ?? [])
            for resource in session.binding?.monitor_resources ?? [] { ids.insert(resource.account_id) }
            for id in ids { byAccount[id, default: []].append(session) }
        }
        sessionsByAccount = byAccount
        sessionByID = Dictionary(sessions.map { ($0.id, $0) }, uniquingKeysWith: { first, _ in first })
        recoveryJobs = (snapshot?.jobs ?? []).filter { ($0.kind ?? "").isEmpty }
            .sorted { (trayDate($0.created_at) ?? .distantPast) > (trayDate($1.created_at) ?? .distantPast) }

        var next: [String: (job: TrayJob, at: Date)] = [:]
        var tests: [String: (job: TrayJob, at: Date)] = [:]
        var delivered: [String: (job: TrayJob, at: Date)] = [:]
        for job in snapshot?.jobs ?? [] {
            if job.delivery?.isEmpty == false {
                let at = trayDate(job.updated_at) ?? .distantPast
                if let current = delivered[job.session_id], current.at >= at {} else { delivered[job.session_id] = (job, at) }
            }
            if job.active {
                let at = trayDate(job.not_before) ?? .distantFuture
                if let current = next[job.session_id], current.at <= at {} else { next[job.session_id] = (job, at) }
            }
            if job.kind == "test" {
                let at = trayDate(job.created_at) ?? .distantPast
                if let current = tests[job.session_id], current.at >= at {} else { tests[job.session_id] = (job, at) }
            }
        }
        nextJob = next.mapValues(\.job)
        latestTest = tests.mapValues(\.job)
        lastDelivery = delivered.mapValues(\.job)

        var recent: [String: [TrayAudit]] = [:]
        for item in (snapshot?.audit ?? []).reversed() where recent[item.session_id, default: []].count < Self.eventLimit {
            recent[item.session_id, default: []].append(item)
        }
        events = recent
    }
}
