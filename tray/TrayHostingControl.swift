import Foundation

// Two independent controls, neither of which stops monitoring:
// - default hosting (default_host_mode) applies only to sessions that follow the
//   global rule; a session's own hosted / monitor / off choice always wins;
// - the emergency stop (policy.paused) halts every automatic send at once.
// Nothing is ever sent without the automation consent recorded by the daemon.
extension TrayRuntime {
    static let emergencyPausedKey = "QSOEmergencyPaused"
    private static let hostingModelKey = "QSOHostingModel4013"
    private static let legacyGuardKey = "QSOGuardEnabled"

    var emergencyDesired: Bool {
        get { UserDefaults.standard.bool(forKey: Self.emergencyPausedKey) }
        set { UserDefaults.standard.set(newValue, forKey: Self.emergencyPausedKey) }
    }
    var defaultHosted: Bool { ["", "hosted"].contains(snapshot?.defaultHostMode ?? "") }
    var autoSendArmed: Bool { localServiceFresh && !paused && automationConsented }
    var liveSessions: [TraySession] { sessions.filter { !$0.isArchived } }
    var hostedSessionCount: Int { liveSessions.filter { $0.host?.mode == "hosted" || $0.host?.mode == "legacy" }.count }
    var monitoredSessionCount: Int { liveSessions.filter { $0.host?.mode == "monitor" }.count }
    var followingSessionCount: Int { liveSessions.filter { ($0.host_mode ?? "").isEmpty }.count }
    var hostingProblemCount: Int { liveSessions.filter { $0.host?.mode == "hosted" && $0.host?.ready == false }.count }

    // One sentence for "will anything be sent automatically right now, and why not".
    var hostingSummary: String {
        if !localServiceFresh { return "等待本地服务" }
        if paused { return "已紧急暂停 · 所有会话只监控" }
        if !automationConsented { return "尚未允许自动继续 · 只监控" }
        let ready = hostedReadyCount
        if ready == 0 { return defaultHosted ? "智能托管 · 暂无可自动继续的会话" : "默认仅监控 · 没有单独开启托管的会话" }
        return defaultHosted ? "智能托管 · \(ready) 个会话可自动继续" : "默认仅监控 · \(ready) 个会话单独托管"
    }

    // Menu-bar symbol: stopped, needs you, sending automatically, or only watching.
    var statusSymbol: String {
        if !localServiceFresh || !proxyConnected { return "bolt.horizontal.circle" }
        if paused { return "pause.circle" }
        if attentionCount > 0 { return "exclamationmark.circle" }
        return autoSendArmed && hostedReadyCount > 0 ? "bolt.horizontal.circle.fill" : "eye.circle"
    }

    // An earlier global master switch meant "nothing is sent". Its off position becomes
    // default-monitor (follow sessions stay quiet) instead of a global stop, so a
    // session explicitly set to hosted works on its own as asked.
    func hostingMigrationPatch(_ received: TraySnapshot) -> [String: JSONValue] {
        let defaults = UserDefaults.standard
        guard !defaults.bool(forKey: Self.hostingModelKey) else { return [:] }
        guard defaults.object(forKey: Self.legacyGuardKey) != nil, !defaults.bool(forKey: Self.legacyGuardKey),
              ["", "hosted"].contains(received.defaultHostMode) else { return [:] }
        return ["default_host_mode": .string(TrayHostMode.monitor.rawValue)]
    }
    func finishHostingMigration() {
        UserDefaults.standard.set(true, forKey: Self.hostingModelKey)
        UserDefaults.standard.removeObject(forKey: Self.legacyGuardKey)
    }

    private func hostingControlReady() -> TraySnapshot? {
        guard !connecting, desktopExecutionReady, !switchingDaemon else { error = "后台正在启动或切换，请稍后再试"; return nil }
        guard localServiceFresh, let snapshot else { error = "本地状态过期，暂不能切换托管"; return nil }
        return snapshot
    }
    // Default hosting only changes sessions that follow the global rule.
    func setDefaultHosting(_ hosted: Bool) async {
        guard let snapshot = hostingControlReady() else { return }
        let mode = hosted ? TrayHostMode.hosted : .monitor
        guard defaultHosted != hosted else { return }
        do {
            _ = try await patchPolicy(["default_host_mode": .string(mode.rawValue)], expectedDigest: snapshot.policy_digest)
            await refresh()
            if hosted && !automationConsented { notice = "已设为默认托管；授权「允许自动继续」之前仍只监控。" }
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "默认托管未确认，请检查当前状态。" }
    }
    // Consent is stamped by the daemon; the client only asks for it.
    func grantAutomationConsent() async -> Bool {
        guard let snapshot = hostingControlReady() else { return false }
        do {
            _ = try await patchPolicy(["automation_consent": .bool(true)], expectedDigest: snapshot.policy_digest)
            await refresh()
            return true
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "授权未保存，请重试"; return false }
    }
    // Revoking stops every automatic send without touching per-session choices.
    func revokeAutomationConsent() async {
        guard let snapshot = hostingControlReady() else { return }
        do {
            _ = try await patchPolicy(["automation_consent": .bool(false)], expectedDigest: snapshot.policy_digest)
            await refresh()
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "撤销未确认，请刷新后查看" }
    }
    // The emergency stop; monitoring continues and queued recoveries wait.
    func setEmergencyPause(_ stop: Bool) async {
        guard let snapshot = hostingControlReady() else { return }
        guard snapshot.paused != stop else { emergencyDesired = stop; return }
        do {
            _ = try await patchPolicy(["paused": .bool(stop)], expectedDigest: snapshot.policy_digest)
            emergencyDesired = stop
            await refresh()
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "暂停开关未确认，请检查当前状态。" }
    }
}
