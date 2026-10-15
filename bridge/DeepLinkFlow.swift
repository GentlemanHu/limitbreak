import AppKit
import ApplicationServices
import Foundation

// Hosted desktop sessions. Navigation is Claude's own session link; identity
// is the link's session id in the renderer URL (or the exact window title); the
// composer is Claude's focused text area. Every check is repeated immediately
// before writing and again before Send. Nothing is typed by key events.
extension NativeBridge {
    static let sendLabels: Set<String> = ["Send", "发送", "Send message", "发送消息"]

    func validateDeepLink(_ binding: DesktopBinding) throws -> BridgeResult {
        _ = try claude(allowHidden: true)
        _ = try linkHandler(binding)
        try timeGate()
        return result("target_ready", "会话链接与 Claude 处理程序已核验；未导航、未输入。", ok: true)
    }

    func locate(_ binding: DesktopBinding) -> BridgeResult {
        AXCallGate.withGate({ self.alive }) {
            do {
                try binding.validateIdentity()
                guard binding.isDeepLinkTarget else { throw BridgeFailure("locate_unavailable", "该会话没有可用的桌面深链接。") }
                try environment()
                _ = try open(binding, waitFrontmost: false)
                return result("located", "已在 Claude 中打开该会话；未输入任何内容。", ok: true)
            } catch { return failure(error) }
        }
    }

    // Claude Desktop starts a session's Claude Code process only when the session is
    // opened. Wake opens the session's own link in the background so the silent
    // message socket appears; it never types, presses anything or needs Accessibility.
    // The session becomes Claude's visible one, so the same idle, quiet and lease
    // checks as a send apply, and the user's app is put back in front afterwards.
    func wake(_ binding: DesktopBinding) -> BridgeResult {
        AXCallGate.withGate({ self.alive }) {
            do {
                try binding.validateIdentity()
                guard binding.isDeepLinkTarget else { throw BridgeFailure("wake_unavailable", "该会话没有可用的桌面深链接。") }
                try consoleGate()
                guard try inputIdle() >= Double(userIdleSeconds) else {
                    throw BridgeFailure("user_active", "用户正在操作桌面或空闲时间不足；稍后再唤醒会话。")
                }
                try quietGate()
                try eligibility()
                try timeGate()
                let previous = previousFront()
                _ = try claude(allowHidden: true)
                _ = try open(binding, waitFrontmost: false, activates: false)
                // Claude may still bring itself forward while handling the link; watch briefly.
                var restored = false
                let end = min(deadline, ProcessInfo.processInfo.systemUptime + 1.5)
                while ProcessInfo.processInfo.systemUptime < end {
                    if !restored, restore(previous) { restored = true }
                    Thread.sleep(forTimeInterval: 0.1)
                }
                return result("woken", restored ? "已在后台打开会话以启动其进程，并把焦点还给原来的应用；未输入任何内容。" : "已在后台打开会话以启动其进程；未输入任何内容。", ok: true)
            } catch { return failure(error) }
        }
    }

    private func linkHandler(_ binding: DesktopBinding) throws -> (URL, URL) {
        guard let link = URL(string: binding.deepLink), link.scheme == "claude" else {
            throw BridgeFailure("invalid_binding", "会话链接无效。")
        }
        guard let app = NSWorkspace.shared.urlForApplication(withBundleIdentifier: Self.bundleID) else {
            throw BridgeFailure("app_not_installed", "未找到 Claude 桌面应用。")
        }
        guard let handler = NSWorkspace.shared.urlForApplication(toOpen: link),
              handler.standardizedFileURL.resolvingSymlinksInPath() == app.standardizedFileURL.resolvingSymlinksInPath() else {
            throw BridgeFailure("deeplink_handler_mismatch", "claude:// 链接的系统处理程序不是 Claude 桌面应用；拒绝打开。")
        }
        return (link, app)
    }

    private final class OpenOutcome: @unchecked Sendable {
        private let lock = NSLock()
        private var app: NSRunningApplication?
        private var finished = false
        func set(_ value: NSRunningApplication?) { lock.lock(); app = value; finished = true; lock.unlock() }
        func get() -> (Bool, NSRunningApplication?) { lock.lock(); defer { lock.unlock() }; return (finished, app) }
    }

    func open(_ binding: DesktopBinding, waitFrontmost: Bool, activates: Bool = true) throws -> NSRunningApplication {
        let (link, appURL) = try linkHandler(binding)
        try timeGate()
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = activates
        configuration.addsToRecentItems = false
        configuration.promptsUserIfNeeded = false
        let outcome = OpenOutcome()
        NSWorkspace.shared.open([link], withApplicationAt: appURL, configuration: configuration) { app, _ in outcome.set(app) }
        let end = min(deadline, ProcessInfo.processInfo.systemUptime + 6)
        var opened: NSRunningApplication?
        while ProcessInfo.processInfo.systemUptime < end {
            try timeGate()
            let (finished, app) = outcome.get()
            if finished { opened = app; break }
            Thread.sleep(forTimeInterval: 0.05)
        }
        guard let app = opened, app.bundleIdentifier == Self.bundleID else {
            throw BridgeFailure("navigation_failed", "Claude 未确认打开该会话链接。")
        }
        guard waitFrontmost else { return app }
        let front = min(deadline, ProcessInfo.processInfo.systemUptime + 3)
        while ProcessInfo.processInfo.systemUptime < front {
            try timeGate()
            if app.isActive, NSWorkspace.shared.frontmostApplication?.processIdentifier == app.processIdentifier { return app }
            Thread.sleep(forTimeInterval: 0.1)
        }
        throw BridgeFailure("app_not_frontmost", "打开会话链接后 Claude 未成为前台应用。")
    }

    private func appElement(_ app: NSRunningApplication) throws -> AXUIElement {
        try AXCallGate.require()
        let root = AXUIElementCreateApplication(app.processIdentifier)
        guard AXUIElementSetMessagingTimeout(root, 0.25) == .success else {
            throw BridgeFailure("ax_unavailable", "无法设置 Claude AX 超时。")
        }
        // Chromium-based apps expose their full tree only to assistive clients that ask.
        _ = AXUIElementSetAttributeValue(root, "AXManualAccessibility" as CFString, kCFBooleanTrue)
        return root
    }

    private func focusedWindow(_ root: AXUIElement) throws -> AXUIElement {
        guard let window = AXTree.element(root, kAXFocusedWindowAttribute as String),
              AXTree.string(window, kAXRoleAttribute as String) == "AXWindow",
              AXTree.bool(window, kAXMinimizedAttribute as String) == false else {
            throw BridgeFailure("window_unverified", "无法确认 Claude 当前聚焦窗口。")
        }
        return window
    }

    // Returns how the open session was proven to be the bound one.
    private func evidence(_ window: AXUIElement, _ binding: DesktopBinding) throws -> String {
        let id = binding.hostSessionID
        var queue: [(AXUIElement, Int)] = [(window, 0)]
        var visited = 0
        var urls: [String] = []
        while !queue.isEmpty, visited < 400 {
            try AXCallGate.require()
            let (element, depth) = queue.removeFirst()
            visited += 1
            let role = AXTree.string(element, kAXRoleAttribute as String) ?? ""
            if role == "AXWebArea" {
                if let raw = AXTree.raw(element, kAXURLAttribute as String), CFGetTypeID(raw) == CFURLGetTypeID(), let url = raw as? URL {
                    urls.append(url.absoluteString)
                }
                continue // The page content itself is never enumerated.
            }
            guard depth < 12, let children = AXTree.raw(element, kAXChildrenAttribute as String) as? [AXUIElement] else { continue }
            queue += children.prefix(64).map { ($0, depth + 1) }
        }
        if urls.contains(where: { $0.contains(id) }) { return "链接" }
        if let other = urls.lazy.compactMap({ url in url.range(of: #"local_[0-9a-f]{8}"#, options: .regularExpression).map { String(url[$0]) } }).first {
            // Right after the link opens, the page may still show the previous session; the caller retries.
            throw BridgeFailure("identity_mismatch", "Claude 当前显示的是另一个会话（\(other)…，目标是 \(id.prefix(14))…）。")
        }
        if !binding.targetTitle.isEmpty, AXTree.string(window, kAXTitleAttribute as String) == binding.targetTitle { return "标题" }
        throw BridgeFailure("identity_unverified", "打开链接后无法证明当前显示的就是目标会话。")
    }

    private func composer(_ root: AXUIElement, in window: AXUIElement) throws -> AXUIElement {
        guard let input = AXTree.element(root, kAXFocusedUIElementAttribute as String),
              AXTree.string(input, kAXRoleAttribute as String) == "AXTextArea",
              AXTree.string(input, kAXSubroleAttribute as String) != "AXSecureTextField",
              let owner = AXTree.element(input, kAXWindowAttribute as String), AXTree.equal(owner, window),
              AXTree.bool(input, kAXEnabledAttribute as String) == true, AXTree.settable(input, kAXValueAttribute as String) else {
            throw BridgeFailure("input_unverified", "Claude 输入框未获得焦点或不可编辑；不会猜测输入位置。")
        }
        return input
    }

    // When Claude does not put focus in the composer after navigation, focus the
    // window's single editable text area. Only roles and flags are read; text
    // values are never queried. More than one candidate is refused.
    private func focusComposer(in window: AXUIElement) throws {
        var queue: [(AXUIElement, Int)] = [(window, 0)]
        var found: [AXUIElement] = []
        var visited = 0
        while !queue.isEmpty, visited < 4000 {
            try AXCallGate.require()
            let (element, depth) = queue.removeFirst()
            visited += 1
            let role = AXTree.string(element, kAXRoleAttribute as String) ?? ""
            if role == "AXTextArea" {
                if AXTree.string(element, kAXSubroleAttribute as String) != "AXSecureTextField",
                   AXTree.bool(element, kAXEnabledAttribute as String) == true,
                   AXTree.settable(element, kAXValueAttribute as String) { found.append(element) }
                continue
            }
            if ["AXStaticText", "AXImage", "AXButton", "AXLink"].contains(role) { continue }
            guard depth < 40, let children = AXTree.raw(element, kAXChildrenAttribute as String) as? [AXUIElement] else { continue }
            queue += children.prefix(200).map { ($0, depth + 1) }
        }
        guard found.count == 1 else {
            throw BridgeFailure(found.isEmpty ? "input_unverified" : "input_ambiguous", found.isEmpty ? "未找到可编辑的 Claude 输入框。" : "窗口中存在多个可编辑输入框；不会猜测。")
        }
        try mutationGate()
        guard AXUIElementSetAttributeValue(found[0], kAXFocusedAttribute as CFString, kCFBooleanTrue) == .success else {
            throw BridgeFailure("input_unverified", "无法让 Claude 输入框获得焦点。")
        }
    }

    private func label(_ element: AXUIElement) -> String? {
        for name in [kAXTitleAttribute as String, kAXDescriptionAttribute as String] {
            if let text = AXTree.string(element, name), Self.sendLabels.contains(text) { return text }
        }
        return nil
    }

    // The nearest ancestor of the composer that contains exactly one Send control.
    private func sendButton(near input: AXUIElement) throws -> AXUIElement {
        var ancestor = input
        for _ in 0..<8 {
            guard let parent = AXTree.element(ancestor, kAXParentAttribute as String) else { break }
            ancestor = parent
            var found: [AXUIElement] = []
            var queue: [(AXUIElement, Int)] = [(ancestor, 0)]
            var visited = 0
            while !queue.isEmpty {
                try AXCallGate.require()
                guard visited < 300 else { throw BridgeFailure("send_unavailable", "输入框附近结构过大；不会扩大搜索。") }
                let (element, depth) = queue.removeFirst()
                visited += 1
                let role = AXTree.string(element, kAXRoleAttribute as String) ?? ""
                if role == "AXButton" {
                    if label(element) != nil, AXTree.pressable(element) { found.append(element) }
                    continue
                }
                if ["AXScrollArea", "AXWebArea", "AXStaticText", "AXTextArea"].contains(role) && depth > 0 { continue }
                guard depth < 8, let children = AXTree.raw(element, kAXChildrenAttribute as String) as? [AXUIElement] else { continue }
                queue += children.prefix(64).map { ($0, depth + 1) }
            }
            if found.count == 1 { return found[0] }
            if found.count > 1 { throw BridgeFailure("send_ambiguous", "输入框附近存在多个发送控件。") }
        }
        throw BridgeFailure("send_unavailable", "找不到唯一的 Send/发送 控件。")
    }

    private struct Target { let app: NSRunningApplication; let root: AXUIElement; let window: AXUIElement; let input: AXUIElement }

    private func verify(_ target: Target, _ binding: DesktopBinding) throws {
        try foreground(target.app)
        guard try claude().processIdentifier == target.app.processIdentifier else { throw BridgeFailure("identity_unverified", "Claude 进程已变化。") }
        let window = try focusedWindow(target.root)
        guard AXTree.equal(window, target.window) else { throw BridgeFailure("identity_unverified", "目标窗口已变化。") }
        _ = try evidence(window, binding)
        guard AXTree.equal(try composer(target.root, in: window), target.input) else { throw BridgeFailure("input_changed", "目标输入框已变化。") }
    }

    func performDeepLinkResume(_ request: ResumeRequest) throws -> BridgeResult {
        let binding = request.binding
        try request.validate(); try mutationGate()
        let app = try open(binding, waitFrontmost: true)
        try foreground(app)
        let root = try appElement(app)
        var verified: Target?
        var proof = ""
        var previous: (AXUIElement, AXUIElement)?
        var focusAttempts = 0
        var lastFailure: BridgeFailure?
        // Claude starts the session's engine on navigation; observed: the page switched ~2 s after a 1 s check gave up.
        let end = min(deadline, ProcessInfo.processInfo.systemUptime + 15)
        while ProcessInfo.processInfo.systemUptime < end {
            try timeGate(); try foreground(app)
            do {
                let window = try focusedWindow(root)
                let how = try evidence(window, binding)
                let input: AXUIElement
                do {
                    input = try composer(root, in: window)
                } catch let error as BridgeFailure where error.code == "input_unverified" && focusAttempts < 2 {
                    // Identity is proven; give the page a moment, then focus its composer.
                    focusAttempts += 1
                    if focusAttempts == 2 { try focusComposer(in: window) }
                    throw error
                }
                if let previous, AXTree.equal(previous.0, window), AXTree.equal(previous.1, input) {
                    verified = Target(app: app, root: root, window: window, input: input); proof = how; break
                }
                previous = (window, input)
            } catch let error as BridgeFailure where ["window_unverified", "identity_unverified", "identity_mismatch", "input_unverified"].contains(error.code) {
                previous = nil
                lastFailure = error
            }
            Thread.sleep(forTimeInterval: 0.25)
        }
        guard let target = verified else {
            throw lastFailure ?? BridgeFailure("identity_unverified", "打开链接后无法稳定核验目标会话和唯一输入框。")
        }
        guard AXTree.composerEmpty(target.input) else {
            throw BridgeFailure("input_not_empty", "输入框里有你的草稿（或内容无法读取）；不会覆盖草稿。")
        }
        try mutationGate() // Fresh authenticated generation/lease immediately before writing.
        try verify(target, binding)
        guard AXTree.composerEmpty(target.input) else { throw BridgeFailure("input_changed", "写入前输入已变化。") }
        try timeGate()
        draftWritten = true
        guard AXUIElementSetAttributeValue(target.input, kAXValueAttribute as CFString, request.message as CFString) == .success else {
            throw BridgeFailure("write_unverified", "AX 写入被 Claude 拒绝。")
        }
        // The editor applies the value asynchronously; read it back until it settles.
        let settle = min(deadline, ProcessInfo.processInfo.systemUptime + 1.5)
        while !AXTree.composerHolds(target.input, request.message) {
            guard ProcessInfo.processInfo.systemUptime < settle else { throw BridgeFailure("write_unverified", "AX 写入未完整确认。") }
            try AXCallGate.require()
            Thread.sleep(forTimeInterval: 0.1)
        }
        var send: AXUIElement?
        let ready = min(deadline, ProcessInfo.processInfo.systemUptime + 2)
        while ProcessInfo.processInfo.systemUptime < ready {
            try timeGate()
            if let button = try? sendButton(near: target.input), AXTree.bool(button, kAXEnabledAttribute as String) == true { send = button; break }
            Thread.sleep(forTimeInterval: 0.1)
        }
        guard let send else { throw BridgeFailure("send_unavailable", "写入后发送控件未就绪。") }
        try mutationGate() // Never trusts the earlier write eligibility for Send.
        try verify(target, binding)
        guard AXTree.equal(try sendButton(near: target.input), send),
              AXTree.composerHolds(target.input, request.message),
              AXTree.bool(send, kAXEnabledAttribute as String) == true else {
            throw BridgeFailure("input_changed", "最终提交检查失败。")
        }
        try timeGate()
        submitted = true // Any Send attempt is permanently non-retryable.
        guard AXUIElementPerformAction(send, kAXPressAction as CFString) == .success else {
            throw BridgeFailure("submitted_uncertain", "Send 结果不确定。")
        }
        let confirm = min(deadline, ProcessInfo.processInfo.systemUptime + 5)
        while ProcessInfo.processInfo.systemUptime < confirm {
            try AXCallGate.require()
            if AXTree.composerEmpty(target.input) {
                return result("submitted", "已在目标会话发送（身份证据：\(proof)）；同一输入框已清空，不保证模型已执行。", ok: true)
            }
            Thread.sleep(forTimeInterval: 0.15)
        }
        throw BridgeFailure("submitted_uncertain", "无法确认提交后状态。")
    }
}
