import AppKit
import ApplicationServices
import Foundation
import SwiftUI
import IOKit.pwr_mgt

@MainActor
final class TrayRuntime: ObservableObject {
    @Published var snapshot: TraySnapshot? { didSet { index = TraySnapshotIndex(snapshot) } }
    private(set) var index = TraySnapshotIndex(nil)
    @Published var error = ""
    @Published var notice = ""
    @Published var connecting = false
    @Published var configured = false
    @Published var proxyURL = "http://127.0.0.1:8317"
    @Published var now = Date()
    @Published var awake = false
    @Published var hooksInstalled = false
    @Published var hookStatus = "尚未接入会话事件"
    @Published var hookStalePaths = 0
    @Published var accessibilityObservation = GuardPermissionObservation()
    @Published var daemonRecoveryStatus = "尚未启动自有后台"
    private var appIdentity: GuardAppIdentity?
    private var desktopServer: DesktopIPCServer?
    private let daemonMonitor = GuardDaemonMonitor()
    private var intentionalStops = Set<ObjectIdentifier>()
    private var shuttingDown = false
    private var recoveryAttempts = 0
    private var recoveryTask: Task<Void, Never>?
    private var daemonTransition = false
    private var recoveringDaemon = false
    // The daemon always starts paused; the user's emergency stop is restored only after the app owns and arms it.
    private var settled = false
    var switchingDaemon: Bool { daemonTransition || recoveringDaemon }
    var showWorkbench: ((WorkbenchSection) -> Void)?
    var showRecoveryWindow: (() -> Void)?
    // A session another page asked the session list to select.
    @Published var focusSession: String?
    var daemonRunning: Bool { child?.isRunning == true }
    var desktopExecutionReady: Bool { daemonMonitor.authorized }
    // Existence only; the key file's contents are never read by the UI.
    var hasSavedManagementKey: Bool { FileManager.default.fileExists(atPath: dataDirectory.appendingPathComponent("management-key").path) }
    var proxySource: GuardSource? { (snapshot?.sources ?? []).first { $0.type == "cliproxyapi" } ?? snapshot?.sources?.first }
    var localServiceFresh: Bool { daemonRunning && now.timeIntervalSince(lastReceived) < 12 }
    private var child: Process?
    private var timer: Timer?
    private var assertion: IOPMAssertionID = 0
    private var refreshing = false
    private var lastReceived = Date.distantPast
    private(set) var launchedAt = Date.distantPast
    private var lastSnapshotData = Data()
    private var token = ""
    private var listen = "127.0.0.1:8329"
    var resources: URL { Bundle.main.resourceURL! }
    var dataDirectory: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("Quota Session Guard", isDirectory: true)
    }
    var configURL: URL { dataDirectory.appendingPathComponent("config.json") }
    var daemonURL: URL { URL(string: "http://\(listen)")! }
    var sessions: [TraySession] { snapshot?.sessions ?? [] }
    var activeJobs: [TrayJob] { (snapshot?.jobs ?? []).filter(\.active) }
    var waitingCount: Int { sessions.filter { ["COOLDOWN", "READY_TO_RESUME", "VERIFYING", "DISPATCHING"].contains($0.state) }.count }
    var attentionCount: Int { sessions.filter { $0.state == "NEEDS_ATTENTION" }.count }
    var hostedReadyCount: Int { sessions.filter { $0.host?.ready == true && !$0.isArchived }.count }
    var availableCount: Int { (snapshot?.accounts ?? []).filter { $0.state == "allowed" && isFresh($0) }.count }
    var enabledAccountCount: Int { (snapshot?.accounts ?? []).filter { $0.disabled != true }.count }
    var availableGroupCount: Int {
        var count = 0
        for account in snapshot?.accounts ?? [] where account.disabled != true && account.quota_error?.isEmpty != false {
            for group in account.groups ?? [] where group.state == "allowed" && isFresh(group) { count += 1 }
        }
        return count
    }
    var automationConsented: Bool { snapshot?.automationConsented == true }
    var accessibilityGranted: Bool { accessibilityObservation.state == .granted }
    func isFresh(_ account: TrayAccount) -> Bool { evidenceFresh(account.checked_at) }
    func isFresh(_ group: TrayQuotaGroup) -> Bool { evidenceFresh(group.checked_at) }
    private func evidenceFresh(_ timestamp: String) -> Bool {
        guard localServiceFresh, let checked = trayDate(timestamp), let seconds = snapshot?.policy["freshness_seconds"]?.number else { return false }
        return checked <= now && now.timeIntervalSince(checked) <= seconds
    }
    func job(for session: TraySession) -> TrayJob? { index.nextJob[session.id] }
    func lastActive(_ session: TraySession) -> Date? { index.lastActive[session.id] }
    // The next time the daemon looks at a waiting session; a deadline already
    // passed is being handled now, so it falls through to the next quota poll.
    var nextCheck: Date? {
        let jobs = (snapshot?.jobs ?? []).filter { ["COOLDOWN", "READY_TO_RESUME"].contains($0.state) }.compactMap { trayDate($0.not_before) }
        let polls = (snapshot?.accounts ?? []).filter { $0.disabled != true }.compactMap { trayDate($0.next_check_at) }
        return (jobs + polls).filter { $0 > now }.min()
    }
    var proxyConnected: Bool { child?.isRunning == true && now.timeIntervalSince(lastReceived) < 12 && snapshot?.capabilities["proxy_connected"]?.bool == true }
    // A fresh daemon reports "not connected" until its first quota read finishes; that wait is not a failure.
    var sourcePhase: TraySourcePhase {
        if !configured { return .unconfigured }
        if proxyConnected { return .connected }
        let failedThisRun = (snapshot?.sources ?? []).contains { $0.state == "error" && (trayDate($0.checked_at) ?? .distantPast) >= launchedAt }
        if failedThisRun || !daemonRunning || now.timeIntervalSince(launchedAt) > Self.firstQuotaPatience { return .failed }
        return snapshot == nil ? .starting : .checking
    }
    var sourceProblem: String {
        let source = (snapshot?.sources ?? []).first { $0.state == "error" }?.error ?? ""
        let message = snapshot?.capabilities["message"]?.string ?? ""
        let detail = [source, message].first { !$0.isEmpty && $0 != "read-only quota monitoring" } ?? ""
        return detail.isEmpty ? "没有读到额度。确认 CLIProxyAPI 已启动、管理密钥正确。" : "没有读到额度（\(detail)）。确认 CLIProxyAPI 已启动、管理密钥正确。"
    }
    var secondsSinceLaunch: Int { max(0, Int(now.timeIntervalSince(launchedAt))) }
    private static let firstQuotaPatience: TimeInterval = 60
    var paused: Bool { snapshot?.paused ?? true }
    var title: String {
        if !configured { return "LimitBreak" }
        if child?.isRunning != true { return "额度 · 离线" }
        if [.starting, .checking].contains(sourcePhase) { return "额度 · 连接中" }
        if !proxyConnected { return "额度 · 未连接" }
        if attentionCount > 0 { return "额度 · 需处理" }
        if paused { return "额度 · 已暂停" }
        if !automationConsented || hostedReadyCount == 0 { return "额度 · 监控" }
        if let nextCheck, waitingCount > 0 { return "额度 " + trayDuration(nextCheck.timeIntervalSince(now)) }
        return "额度 · 托管"
    }

    func start() async {
        do {
            guard appIdentity == nil, !shuttingDown else { return }
            appIdentity = try GuardAppIdentity(directory: dataDirectory)
            checkAccessibility()
            do { try refreshHookEntry() } catch { self.error = (error as? RuntimeFailure)?.message ?? "无法更新稳定 Hook 入口" }
            detectHooks()
            if !FileManager.default.fileExists(atPath: configURL.path) {
                let process = Process()
                process.executableURL = resources.appendingPathComponent("bin/orchestrator")
                process.arguments = ["init", "--config", configURL.path]
                process.currentDirectoryURL = resources
                process.standardOutput = FileHandle.nullDevice
                process.standardError = FileHandle.nullDevice
                try process.run()
                for _ in 0..<60 {
                    if !process.isRunning { break }
                    try await Task.sleep(nanoseconds: 50_000_000)
                }
                guard !process.isRunning else { process.terminate(); throw RuntimeFailure("初始化超时，请重新打开应用") }
                guard process.terminationStatus == 0 else { throw RuntimeFailure("无法初始化本机配置") }
                var config = try readConfig()
                config["listen"] = listen
                try writeConfig(config)
            }
            var config = try readConfig()
            config.removeValue(forKey: "web_dir") // The app has no browser UI.
            config["bridge_path"] = "" // Production execution belongs exclusively to the main App IPC.
            config["desktop_socket"] = appIdentity!.socketURL.path
            config["desktop_token_file"] = appIdentity!.tokenURL.path
            try writeConfig(config)
            token = config["token"] as? String ?? ""
            listen = config["listen"] as? String ?? listen
            proxyURL = config["proxy_url"] as? String ?? proxyURL
            guard token.count >= 32, !token.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }),
                  let endpoint = URL(string: "http://\(listen)"), endpoint.host == "127.0.0.1", endpoint.user == nil,
                  endpoint.password == nil, endpoint.path.isEmpty, endpoint.query == nil, endpoint.fragment == nil,
                  let port = endpoint.port, (1...65535).contains(port) else { throw RuntimeFailure("私有配置无效，请在连接设置中重新配置") }
            let monitor = daemonMonitor
            let server = DesktopIPCServer(identity: appIdentity!, listen: listen, daemonToken: token, daemonAvailable: { monitor.authorized })
            try server.start()
            desktopServer = server
            let keyURL = dataDirectory.appendingPathComponent("management-key")
            configured = FileManager.default.fileExists(atPath: keyURL.path) || !(config["sources"] as? [[String: Any]] ?? []).isEmpty
            try launch()
            timer?.invalidate()
            var tick = 0
            timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
                Task { @MainActor in
                    guard let self else { return }
                    self.now = Date()
                    tick += 1
                    if tick % 2 == 0 { self.checkAccessibility() }
                    if tick % 3 == 0 { await self.refresh() }
                    if tick % 30 == 0 { self.detectHooks() }
                }
            }
            if let timer { RunLoop.main.add(timer, forMode: .common) }
            try await confirmOwnedDaemonReady()
            await awaitFirstQuota()
        } catch {
            stopOwnedDaemon()
            self.error = (error as? RuntimeFailure)?.message ?? "无法启动本地守护器。检查应用是否完整、数据目录是否可写。"
        }
    }

    func readConfig() throws -> [String: Any] {
        guard let value = try JSONSerialization.jsonObject(with: GuardPrivateStorage.read(configURL)) as? [String: Any] else { throw RuntimeFailure("私有配置格式无效") }
        return value
    }
    func writeConfig(_ config: [String: Any]) throws {
        let data = try JSONSerialization.data(withJSONObject: config, options: [.prettyPrinted, .sortedKeys])
        try GuardPrivateStorage.write(data, to: configURL)
    }
    private func launch() throws {
        guard child?.isRunning != true else { return }
        let process = Process()
        process.executableURL = resources.appendingPathComponent("bin/orchestrator")
        // Paused until this app has armed its own IPC; the user's choice is restored afterwards.
        process.arguments = ["run", "--config", configURL.path, "--startup-paused"]
        process.currentDirectoryURL = resources
        var environment = ProcessInfo.processInfo.environment
        let keyName = (try readConfig())["management_key_env"] as? String ?? "CPA_MANAGEMENT_KEY"
        environment.removeValue(forKey: keyName) // Legacy CPA uses only its private settings file.
        let sources = (try readConfig())["sources"] as? [[String: Any]] ?? []
        for source in sources {
            guard let id = source["id"] as? String, let name = source["credential_env"] as? String, !name.isEmpty,
                  let type = source["type"] as? String, let url = source["url"] as? String else { continue }
            let identity = GuardSourceCredentials.identity(sourceID: id, type: type, url: url, environment: name)
            if let credential = try GuardSourceCredentials.load(identity) { environment[name] = credential }
        }
        process.environment = environment
        process.standardOutput = FileHandle.nullDevice
        daemonMonitor.prepare(process) { [weak self, weak process] status, summary in
            Task { @MainActor in
                guard let self, let process, self.child === process else { return }
                if self.intentionalStops.remove(ObjectIdentifier(process)) != nil || self.shuttingDown { return }
                self.daemonRecoveryStatus = "自有后台退出（\(status)）；\(summary)"
                self.error = self.daemonRecoveryStatus
                self.scheduleDaemonRecovery(blocked: summary.contains("端口占用") || summary.contains("后台所有权"))
            }
        }
        child = process
        snapshot = nil
        settled = false
        lastReceived = .distantPast
        launchedAt = Date()
        do { try process.run() }
        catch { daemonMonitor.abortPrepared(); child = nil; throw RuntimeFailure("自有后台未能启动，未接管其他进程") }
    }
    private func stopOwnedDaemon() {
        daemonMonitor.disarm()
        settled = false
        if let child, child.isRunning {
            intentionalStops.insert(ObjectIdentifier(child))
            child.terminate()
        }
    }
    private func waitForExit(_ attempts: Int) async throws {
        for _ in 0..<attempts {
            if !daemonRunning { return }
            try await Task.sleep(nanoseconds: 100_000_000)
        }
    }
    func restartOwnedDaemon() async {
        guard !daemonTransition, !recoveringDaemon, !connecting, !shuttingDown else { error = "后台正在切换，不能同时重启"; return }
        daemonTransition = true
        defer { daemonTransition = false }
        do {
            try await pauseForConfiguration()
            stopOwnedDaemon()
            try await waitForExit(40)
            guard !daemonRunning else { throw RuntimeFailure("自有后台尚未退出；未强杀或接管其他进程") }
            recoveryAttempts = 0
            try launch()
            try await confirmOwnedDaemonReady()
            await awaitFirstQuota()
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "后台重启未确认；自动继续保持暂停" }
    }
    func shutdown() {
        shuttingDown = true
        recoveryTask?.cancel()
        timer?.invalidate()
        stopOwnedDaemon()
        desktopServer?.stop(); desktopServer = nil
        if awake { IOPMAssertionRelease(assertion); awake = false }
    }
    private func confirmOwnedDaemonReady() async throws {
        guard let generation = daemonMonitor.identity else { throw RuntimeFailure("自有后台未运行") }
        var received: TraySnapshot?
        for _ in 0..<40 {
            guard !shuttingDown, daemonMonitor.identity == generation else { throw RuntimeFailure("自有后台启动身份已变化") }
            if let data = try? await request("/api/snapshot"), let value = try? JSONDecoder().decode(TraySnapshot.self, from: data) { received = value; break }
            try await Task.sleep(nanoseconds: 200_000_000)
        }
        guard let received, received.paused, daemonMonitor.identity == generation, daemonMonitor.arm(generation) else { throw RuntimeFailure("自有后台有界等待后仍未就绪；未授权桌面执行") }
        snapshot = received; lastReceived = Date()
        // Lifting the startup pause is safe without consent: the daemon plans no send until consent exists.
        var patch = hostingMigrationPatch(received)
        if !emergencyDesired { patch["paused"] = .bool(false) }
        if patch.isEmpty {
            finishHostingMigration()
            daemonRecoveryStatus = "自有后台已就绪；保持你上次设置的紧急暂停"
        } else {
            do {
                _ = try await patchPolicy(patch, expectedDigest: received.policy_digest)
                finishHostingMigration()
                await refresh()
                daemonRecoveryStatus = emergencyDesired ? "自有后台已就绪；保持你上次设置的紧急暂停" : "自有后台已就绪；自动继续按各会话的托管设置运行"
            } catch { daemonRecoveryStatus = "自有后台已就绪，但解除启动暂停未确认；保持暂停，可在工具栏手动恢复" }
        }
        settled = true
    }
    // The first quota read takes a few seconds; poll briskly instead of on the 3-second tick so step 1 settles at once.
    private func awaitFirstQuota() async {
        for _ in 0..<60 where !shuttingDown {
            await refresh()
            if ![.starting, .checking].contains(sourcePhase) { return }
            try? await Task.sleep(nanoseconds: 500_000_000)
            now = Date()
        }
    }
    private func scheduleDaemonRecovery(blocked: Bool) {
        guard !recoveringDaemon else { return }
        guard !blocked, !daemonTransition, !shuttingDown, !connecting, recoveryAttempts < 3 else {
            daemonRecoveryStatus += "；自动恢复停止（冲突、配置修改或已达3次上限）"
            return
        }
        recoveryTask?.cancel()
        recoveryTask = Task { [weak self] in
            guard let self else { return }
            self.recoveringDaemon = true
            defer { self.recoveringDaemon = false }
            while self.recoveryAttempts < 3 && !Task.isCancelled && !self.shuttingDown && !self.connecting && !self.daemonTransition {
                self.recoveryAttempts += 1
                do {
                    try await Task.sleep(nanoseconds: UInt64(self.recoveryAttempts) * 1_000_000_000)
                    guard !Task.isCancelled, !self.shuttingDown, !self.daemonRunning else { return }
                    try self.launch()
                    try await self.confirmOwnedDaemonReady()
                    self.error = ""
                    self.notice = "后台异常退出后已自动恢复；没有重发历史指令。"
                    return
                } catch {
                    self.stopOwnedDaemon()
                    self.daemonRecoveryStatus = "第\(self.recoveryAttempts)次后台恢复未确认；桌面执行禁用"
                    self.error = (error as? RuntimeFailure)?.message ?? self.daemonRecoveryStatus
                    // A CAS conflict or unconfirmed pause must not be retried against a newer policy.
                    if self.daemonRunning || self.snapshot != nil { return }
                }
            }
            self.daemonRecoveryStatus = "自有后台恢复已达3次上限，需用户处理；未接管其他后台"
        }
    }

    func connect(url: String, key: String) async {
        guard !connecting, !daemonTransition, !recoveringDaemon else { return }
        do {
            if !(try sourceConfigurations()).isEmpty { error = "当前使用显式多来源配置。请在「来源与适配」修改对应来源；兼容默认连接不会覆盖显式来源。"; return }
        } catch { self.error = "无法读取来源配置，未修改连接。"; return }
        guard let parsed = URLComponents(string: url), parsed.host != nil, parsed.user == nil,
              parsed.password == nil, parsed.query == nil, parsed.fragment == nil,
              parsed.scheme == "https" || parsed.scheme == "http" && ["localhost", "127.0.0.1", "::1"].contains(parsed.host!) else {
            error = "请填写本机代理地址，或使用可信的 HTTPS 地址。"; return
        }
        let key = key.trimmingCharacters(in: .whitespacesAndNewlines)
        let keyURL = dataDirectory.appendingPathComponent("management-key")
        // An empty field keeps the saved key; the saved value is never read back into the UI.
        let keepSavedKey = key.isEmpty && hasSavedManagementKey
        guard keepSavedKey || (!key.isEmpty && key.utf8.count <= 4096 && !key.contains("\n") && !key.contains("\r")) else { error = "请填写有效的管理密钥。"; return }
        connecting = true
        error = ""
        defer { connecting = false }
        do {
            var config = try readConfig()
            config["proxy_url"] = url.trimmingCharacters(in: .whitespacesAndNewlines).trimmingCharacters(in: CharacterSet(charactersIn: "/"))
            config["bridge_path"] = "" // Production execution belongs exclusively to the main App IPC.
            try await pauseForConfiguration()
            stopOwnedDaemon()
            try await waitForExit(30)
            guard child?.isRunning != true else { throw RuntimeFailure("后台尚未退出，请稍后再连接") }
            try writeConfig(config)
            if !keepSavedKey { try GuardPrivateStorage.write(Data(key.utf8), to: keyURL) }
            proxyURL = config["proxy_url"] as? String ?? url
            configured = true
            try launch()
            try await confirmOwnedDaemonReady()
            for _ in 0..<15 {
                try await Task.sleep(nanoseconds: 300_000_000)
                await refresh()
                if proxyConnected { return }
                let detail = snapshot?.capabilities["message"]?.string ?? ""
                if detail.contains("HTTP 401") || detail.contains("HTTP 403") { error = "管理密钥不正确，或代理禁止此管理访问。请核对密钥，不会反复尝试触发封禁。"; return }
            }
            error = "连接尚未确认。确认 CLIProxyAPI 已启动，管理密钥正确；不会自动发送任何消息。"
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "连接设置未保存成功，请重试。" }
    }

    func request(_ path: String, body: Data? = nil, method: String? = nil) async throws -> Data {
        guard child?.isRunning == true else { throw RuntimeFailure("本地守护器未运行") }
        return try await GuardAPIClient(listen: listen, token: token).request(path, body: body, method: method)
    }
    func refresh() async {
        guard !refreshing else { return }
        refreshing = true
        defer { refreshing = false }
        do {
            guard let generation = daemonMonitor.identity else { return }
            let data = try await request("/api/snapshot")
            guard daemonMonitor.identity == generation else { return }
            // An identical payload changes nothing on screen; skip the decode and the full re-render.
            if data != lastSnapshotData || snapshot == nil {
                let received = try await Task.detached(priority: .userInitiated) { try JSONDecoder().decode(TraySnapshot.self, from: data) }.value
                guard daemonMonitor.identity == generation else { return }
                lastSnapshotData = data
                snapshot = received
            }
            lastReceived = Date()
            // Follow pause/unpause made elsewhere (MCP) once startup restoration has finished.
            if let received = snapshot, settled && daemonMonitor.authorized && !daemonTransition && !recoveringDaemon && !connecting { emergencyDesired = received.paused }
            if proxyConnected && error.hasPrefix("连接尚未确认") { error = "" }
        }
        catch { if child?.isRunning != true { self.error = "本地守护器已退出，请重新打开应用。" } }
    }
    func refreshQuota() async {
        do { _ = try await request("/api/refresh", body: Data("{}".utf8)); await refresh() }
        catch { self.error = "额度查询未完成，请检查代理连接。" }
    }
    func pauseForConfiguration() async throws {
        guard daemonRunning else { return }
        await refresh()
        guard localServiceFresh, let snapshot else { throw RuntimeFailure("无法确认调度暂停，未修改来源配置") }
        guard !snapshot.paused else { return }
        let response = try await patchPolicy(["paused": .bool(true)], expectedDigest: snapshot.policy_digest)
        guard response.policy["paused"]?.bool == true else { throw RuntimeFailure("后台暂停未确认，未修改来源配置") }
    }
    func applyConfiguration(_ configuration: [String: Any]) async throws {
        guard !daemonTransition, !recoveringDaemon, !shuttingDown else { throw RuntimeFailure("后台正在切换，请稍后重试") }
        daemonTransition = true
        defer { daemonTransition = false }
        let previous = try readConfig()
        try await pauseForConfiguration()
        stopOwnedDaemon()
        try await waitForExit(40)
        guard child?.isRunning != true else { throw RuntimeFailure("后台尚未停止，请稍后重试") }
        do {
            try writeConfig(configuration)
            snapshot = nil
            try launch()
            try await confirmOwnedDaemonReady()
        } catch {
            stopOwnedDaemon()
            // Never start a second daemon while the app-owned old child is still running.
            try? await waitForExit(40)
            if !daemonRunning {
                try? writeConfig(previous)
                try? launch()
                try? await confirmOwnedDaemonReady()
            }
            throw RuntimeFailure("新来源未能启动，已尝试恢复原配置")
        }
        for _ in 0..<40 {
            try await Task.sleep(nanoseconds: 200_000_000)
            await refresh()
            if snapshot != nil { return }
        }
        throw RuntimeFailure("后台未就绪，自动继续保持暂停")
    }

    func openWorkbench(_ section: WorkbenchSection = .overview) { showWorkbench?(section) }
    func toggleAwake() {
        if awake { IOPMAssertionRelease(assertion); awake = false }
        else {
            awake = IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleSystemSleep as CFString, IOPMAssertionLevel(kIOPMAssertionLevelOn), "Quota Session Guard" as CFString, &assertion) == kIOReturnSuccess
            if !awake { error = "系统防睡眠设置失败" }
        }
    }
}
