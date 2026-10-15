import AppKit
import ApplicationServices
import Darwin
import Foundation

final class NativeBridge {
    private(set) var accessibilityGranted = false
    private(set) var screenLocked = true
    // Shared with the deep-link flow in DeepLinkFlow.swift.
    var submitted = false
    var draftWritten = false
    let deadline: TimeInterval
    private let cancelled: () -> Bool
    let eligibility: () throws -> Void
    let userIdleSeconds: Int
    // 0 disables the gate; otherwise Claude must sit idle this long before a session switch.
    let quietSeconds: Int
    let restoreFocus: Bool

    init(deadline: Date = Date().addingTimeInterval(40), userIdleSeconds: Int = 3, quietSeconds: Int = 0, restoreFocus: Bool = false,
         cancelled: @escaping () -> Bool = { false },
         eligibility: @escaping () throws -> Void = { throw BridgeFailure("execution_unverified", "诊断桥不能证明有效执行租约；禁止发送。") }) {
        self.deadline = ProcessInfo.processInfo.systemUptime + max(0, min(45, deadline.timeIntervalSinceNow))
        self.cancelled = cancelled
        self.eligibility = eligibility
        self.userIdleSeconds = max(1, min(3600, userIdleSeconds))
        self.quietSeconds = max(0, min(3600, quietSeconds))
        self.restoreFocus = restoreFocus
    }
    var alive: Bool { !cancelled() && ProcessInfo.processInfo.systemUptime < deadline }
    func result(_ code: String, _ message: String, ok: Bool = false) -> BridgeResult {
        BridgeResult(ok: ok, submitted: submitted, code: code, message: message,
                     accessibilityGranted: accessibilityGranted, screenLocked: screenLocked)
    }
    func failure(_ error: Error) -> BridgeResult {
        if submitted { return result("submitted_uncertain", "Send 已尝试但提交状态无法确认；禁止自动重发。") }
        if draftWritten { return result("draft_retained", "未尝试 Send；草稿保留，需人工检查，禁止自动覆盖或重发。") }
        if let failure = error as? BridgeFailure { return result(failure.code, failure.message) }
        return result("bridge_error", "原生操作失败；未输出输入正文或 AX 原始内容。")
    }
    func timeGate() throws {
        guard alive else { throw BridgeFailure("cancelled", "请求取消、连接已关闭或操作期限已到。") }
    }
    func environment() throws {
        try consoleGate()
        guard accessibilityGranted else { throw BridgeFailure("accessibility_denied", "主进程未获辅助功能授权；仅用户可申请。") }
    }
    // Active, unlocked console with a lit display. Needs no Accessibility grant.
    func consoleGate() throws {
        try timeGate()
        accessibilityGranted = AXIsProcessTrusted() // Never requests permission.
        screenLocked = true
        guard let session = CGSessionCopyCurrentDictionary() as? [String: Any] else {
            throw BridgeFailure("environment_unverified", "无法核验 GUI console。")
        }
        guard session[kCGSessionOnConsoleKey as String] as? Bool == true,
              session[kCGSessionLoginDoneKey as String] as? Bool == true,
              (session[kCGSessionUserIDKey as String] as? NSNumber)?.uint32Value == getuid() else {
            throw BridgeFailure("console_inactive", "当前用户 console 不活跃。")
        }
        // macOS adds CGSSessionScreenIsLocked only while the screen is locked; on an
        // active, logged-in console its absence means unlocked. A present value
        // that is not a boolean is still refused.
        let lockValue = session["CGSSessionScreenIsLocked"]
        guard lockValue == nil || lockValue is Bool || lockValue is NSNumber else {
            throw BridgeFailure("lock_state_unverified", "系统锁屏状态格式无法识别；拒绝操作。")
        }
        let locked = (lockValue as? Bool) ?? (lockValue as? NSNumber)?.boolValue ?? false
        screenLocked = locked
        guard !locked else { throw BridgeFailure("screen_locked", "屏幕已锁定；不会解锁。") }
        guard CGDisplayIsActive(CGMainDisplayID()) != 0, CGDisplayIsAsleep(CGMainDisplayID()) == 0 else {
            throw BridgeFailure("screen_inactive", "显示器不可用；不会唤醒屏幕。")
        }
    }
    func inputIdle() throws -> Double {
        guard let anyInput = CGEventType(rawValue: UInt32.max) else {
            throw BridgeFailure("idle_state_unverified", "无法核验全部输入事件的空闲状态；拒绝操作。")
        }
        let idle = CGEventSource.secondsSinceLastEventType(.combinedSessionState, eventType: anyInput)
        guard idle.isFinite else { throw BridgeFailure("idle_state_unverified", "无法核验全部输入事件的空闲状态；拒绝操作。") }
        return idle
    }
    func idleGate() throws {
        try environment()
        guard try inputIdle() >= Double(userIdleSeconds) else {
            throw BridgeFailure("user_active", "用户正在操作桌面或空闲时间不足；拒绝抢占输入。")
        }
    }
    // Switching Claude's visible session while the user works in Claude would pull
    // the conversation out from under them, so wait until Claude has sat idle.
    func quietGate() throws {
        guard quietSeconds > 0, NSWorkspace.shared.frontmostApplication?.bundleIdentifier == Self.bundleID else { return }
        let idle = try inputIdle()
        guard idle >= Double(quietSeconds) else {
            throw BridgeFailure("claude_in_use", "你正在使用 Claude；空闲 \(quietSeconds) 秒后再切换会话（已空闲 \(Int(idle)) 秒）。")
        }
    }
    // The app that was in front before LimitBreak opened a session link; nil when it was Claude itself.
    func previousFront() -> NSRunningApplication? {
        guard restoreFocus, let front = NSWorkspace.shared.frontmostApplication, front.bundleIdentifier != Self.bundleID else { return nil }
        return front
    }
    // Puts the user's app back in front if Claude took focus. Never raises Claude or any other app.
    @discardableResult
    func restore(_ previous: NSRunningApplication?) -> Bool {
        guard let previous, !previous.isTerminated,
              NSWorkspace.shared.frontmostApplication?.bundleIdentifier == Self.bundleID else { return false }
        let activate = { previous.activate(options: []) }
        _ = Thread.isMainThread ? activate() : DispatchQueue.main.sync(execute: activate)
        let end = ProcessInfo.processInfo.systemUptime + 1
        while ProcessInfo.processInfo.systemUptime < end {
            if NSWorkspace.shared.frontmostApplication?.processIdentifier == previous.processIdentifier { return true }
            Thread.sleep(forTimeInterval: 0.05)
        }
        // Since macOS 14 a background activate() may be declined; reopening the bundle is honored.
        guard let url = previous.bundleURL else { return false }
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = true
        configuration.addsToRecentItems = false
        configuration.promptsUserIfNeeded = false
        NSWorkspace.shared.openApplication(at: url, configuration: configuration) { _, _ in }
        return true
    }
    func mutationGate() throws {
        try idleGate()
        try eligibility()
        try timeGate()
    }
    static let bundleID = "com.anthropic.claudefordesktop"
    // Identified by bundle id, never by a display name another app could share.
    func claude(allowHidden: Bool = false) throws -> NSRunningApplication {
        try timeGate()
        let candidates = NSWorkspace.shared.runningApplications.filter { !$0.isTerminated && $0.bundleIdentifier == Self.bundleID }
        guard candidates.count == 1, let app = candidates.first else {
            throw BridgeFailure(candidates.isEmpty ? "app_not_running" : "app_ambiguous", "需要唯一已运行的 Claude 桌面应用。")
        }
        guard allowHidden || !app.isHidden else { throw BridgeFailure("app_hidden", "Claude 已隐藏；不会解除隐藏或猜测目标。") }
        return app
    }
    func foreground(_ app: NSRunningApplication) throws {
        try idleGate()
        guard try claude().processIdentifier == app.processIdentifier, app.isActive, !app.isHidden, !app.isTerminated,
              NSWorkspace.shared.frontmostApplication?.processIdentifier == app.processIdentifier else {
            throw BridgeFailure("app_not_frontmost", "Claude 不在前台，拒绝输入。")
        }
    }
    private func windows(_ app: NSRunningApplication) throws -> [AXUIElement] {
        try environment()
        let root = AXUIElementCreateApplication(app.processIdentifier)
        try timeGate()
        guard AXUIElementSetMessagingTimeout(root, 0.2) == .success,
              let raw = AXTree.raw(root, kAXWindowsAttribute as String) as? [AXUIElement], !raw.isEmpty, raw.count <= 16 else {
            throw BridgeFailure("window_unverified", "无法完整枚举有界 Claude 窗口。")
        }
        return raw
    }
    private func trees(_ app: NSRunningApplication, binding: DesktopBinding? = nil) throws -> [AXTree] {
        var result: [AXTree] = []
        for window in try windows(app) {
            try timeGate()
            guard AXTree.string(window, kAXRoleAttribute as String) == "AXWindow" else {
                throw BridgeFailure("window_unverified", "存在未知窗口结构，拒绝猜测。")
            }
            if let binding, !binding.targetWindowIdentifier.isEmpty,
               AXTree.string(window, kAXIdentifierAttribute as String) != binding.targetWindowIdentifier { continue }
            guard AXTree.bool(window, kAXMinimizedAttribute as String) == false, AXTree.bool(window, "AXHidden") != true else { continue }
            result.append(try AXTree(window: window))
        }
        return result
    }
    private func target(_ app: NSRunningApplication, binding: DesktopBinding, allowNavigation: Bool) throws -> AXTree {
        var matches: [AXTree] = []
        var reason: BridgeFailure?
        for tree in try trees(app, binding: binding) {
            do {
                if allowNavigation { _ = try tree.sidebarTarget(binding) }
                else { _ = try tree.identity(binding) }
                matches.append(tree)
            } catch let error as BridgeFailure {
                if ["target_ambiguous", "input_ambiguous"].contains(error.code) { throw error }
                reason = error
            }
        }
        guard matches.count == 1, let tree = matches.first else {
            if matches.isEmpty { throw reason ?? BridgeFailure("target_not_found", "目标窗口或 Pane 不可见／身份未知。") }
            throw BridgeFailure("target_ambiguous", "多个窗口或 Pane 命中目标，拒绝自动操作。")
        }
        return tree
    }
    private func recheck(_ app: NSRunningApplication, window: AXUIElement, binding: DesktopBinding,
                         composer: AXUIElement? = nil, send: AXUIElement? = nil) throws -> AXTree {
        try foreground(app)
        let tree = try target(app, binding: binding, allowNavigation: false)
        guard AXTree.equal(tree.window, window) else { throw BridgeFailure("identity_unverified", "目标窗口已变化。") }
        let pane = try tree.identity(binding)
        if let composer, !AXTree.equal(try tree.composer(in: pane), composer) { throw BridgeFailure("input_changed", "目标输入框已变化。") }
        if let send, !AXTree.equal(try tree.sendButton(in: pane), send) { throw BridgeFailure("send_changed", "发送控件已变化。") }
        try foreground(app)
        return tree
    }
    func probe() -> BridgeResult {
        AXCallGate.withGate({ self.alive }) {
            do { try environment(); _ = try claude(); return result("ready", "环境可用；尚未验证任何绑定目标。", ok: true) }
            catch { return failure(error) }
        }
    }
    func inspect() -> Inspection {
        AXCallGate.withGate({ self.alive }) {
            do {
                try environment()
                let all = try trees(claude())
                var nodes: [SafeNode] = []
                for tree in all {
                    let offset = nodes.count
                    nodes += tree.safeNodes().prefix(120 - min(120, nodes.count)).map {
                        SafeNode(index: $0.index + offset, parent: $0.parent.map { $0 + offset }, role: $0.role,
                                 subrole: $0.subrole, identifier: $0.identifier, title: $0.title, scope: $0.scope)
                    }
                }
                return Inspection(result: result("inspected", "仅输出导航和标题栏 chrome；不输出对话或输入值。", ok: true), nodes: nodes, profile: "strict-chrome-v2")
            } catch { return Inspection(result: failure(error), nodes: [], profile: "strict-chrome-v2") }
        }
    }
    func validateTarget(_ binding: DesktopBinding) -> BridgeResult {
        AXCallGate.withGate({ self.alive }) {
            do {
                try binding.validateIdentity(); try environment()
                if binding.isDeepLinkTarget { return try validateDeepLink(binding) }
                let tree = try target(claude(), binding: binding, allowNavigation: false)
                let pane = try tree.identity(binding)
                let input = try tree.composer(in: pane)
                _ = try tree.sendButton(in: pane)
                guard AXTree.composerEmpty(input) else { throw BridgeFailure("input_not_empty", "目标有草稿或输入状态未知；不会覆盖。") }
                try timeGate()
                return result("target_ready", "只读目标身份与空输入框已核验；未激活、未导航、未发送。", ok: true)
            } catch { return failure(error) }
        }
    }
    func resume(_ request: ResumeRequest) -> BridgeResult {
        AXCallGate.withGate({ self.alive }) {
            let previous = previousFront()
            defer { restore(previous) }
            do {
                // Checked once up front: after LimitBreak brings Claude forward it is, of course, in front.
                try quietGate()
                return request.binding.isDeepLinkTarget ? try performDeepLinkResume(request) : try performResume(request)
            } catch { return failure(error) }
        }
    }
    private func performResume(_ request: ResumeRequest) throws -> BridgeResult {
        try request.validate(); try mutationGate()
        let app = try claude()
        let initial = try target(app, binding: request.binding, allowNavigation: true)
        if !app.isActive {
            try mutationGate()
            let activate = { app.activate(options: []) }
            let activated = Thread.isMainThread ? activate() : DispatchQueue.main.sync(execute: activate)
            guard activated else { throw BridgeFailure("app_not_frontmost", "无法激活已运行 Claude。") }
            let end = min(deadline, ProcessInfo.processInfo.systemUptime + 3)
            while !app.isActive && ProcessInfo.processInfo.systemUptime < end { try timeGate(); Thread.sleep(forTimeInterval: 0.1) }
        }
        try foreground(app)
        // Resume alone is authorized to navigate. Never follows unverified deep links.
        let navigation = try initial.sidebarTarget(request.binding)
        try mutationGate()
        guard AXUIElementPerformAction(navigation, kAXPressAction as CFString) == .success else {
            throw BridgeFailure("navigation_failed", "精确导航 AXPress 未成功。")
        }
        let end = min(deadline, ProcessInfo.processInfo.systemUptime + 8)
        var verified: AXTree?
        var stable = 0
        while ProcessInfo.processInfo.systemUptime < end {
            try timeGate(); try foreground(app)
            do {
                let tree = try recheck(app, window: initial.window, binding: request.binding)
                _ = try tree.composer(in: tree.identity(request.binding))
                stable += 1
                if stable >= 2 { verified = tree; break }
            } catch let error as BridgeFailure where ["identity_unverified", "target_not_found", "target_not_visible", "input_ambiguous"].contains(error.code) { stable = 0 }
            Thread.sleep(forTimeInterval: 0.15)
        }
        guard let verified else { throw BridgeFailure("identity_unverified", "导航后无法稳定核验唯一会话、项目和 Code Pane。") }
        let pane = try verified.identity(request.binding)
        let input = try verified.composer(in: pane)
        _ = try verified.sendButton(in: pane)
        _ = try recheck(app, window: initial.window, binding: request.binding, composer: input)
        guard AXTree.composerEmpty(input), AXTree.bool(input, kAXEnabledAttribute as String) == true,
              AXTree.settable(input, kAXValueAttribute as String) else { throw BridgeFailure("input_not_empty", "输入必须严格为空且可编辑；不会覆盖草稿。") }
        try mutationGate() // Fresh authenticated generation/lease immediately before writing.
        try foreground(app)
        guard AXTree.composerEmpty(input) else { throw BridgeFailure("input_changed", "写入前输入已变化。") }
        try timeGate()
        draftWritten = true
        guard AXUIElementSetAttributeValue(input, kAXValueAttribute as CFString, request.message as CFString) == .success,
              AXTree.composerHolds(input, request.message) else { throw BridgeFailure("write_unverified", "AX 写入未完整确认。") }
        let before = try recheck(app, window: initial.window, binding: request.binding, composer: input)
        let send = try before.sendButton(in: before.identity(request.binding))
        _ = try recheck(app, window: initial.window, binding: request.binding, composer: input, send: send)
        try mutationGate() // Never trusts the earlier write eligibility for Send.
        _ = try recheck(app, window: initial.window, binding: request.binding, composer: input, send: send)
        guard AXTree.composerHolds(input, request.message),
              AXTree.bool(input, kAXEnabledAttribute as String) == true, AXTree.bool(send, kAXEnabledAttribute as String) == true else {
            throw BridgeFailure("input_changed", "最终提交检查失败。")
        }
        try timeGate()
        submitted = true // Any Send attempt is permanently non-retryable.
        guard AXUIElementPerformAction(send, kAXPressAction as CFString) == .success else { throw BridgeFailure("submitted_uncertain", "Send 结果不确定。") }
        let confirmEnd = min(deadline, ProcessInfo.processInfo.systemUptime + 5)
        while ProcessInfo.processInfo.systemUptime < confirmEnd {
            let tree = try recheck(app, window: initial.window, binding: request.binding)
            let current = try tree.composer(in: tree.identity(request.binding))
            guard AXTree.equal(current, input) else { throw BridgeFailure("submitted_uncertain", "提交后输入框已替换。") }
            if AXTree.composerEmpty(current) { return result("submitted", "Send 已尝试且同一输入框已清空；不保证模型已执行。", ok: true) }
            Thread.sleep(forTimeInterval: 0.15)
        }
        throw BridgeFailure("submitted_uncertain", "无法确认提交后状态。")
    }
}
