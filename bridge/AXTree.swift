import ApplicationServices
import Foundation

// This deliberately narrow profile is NOT a claim about any Claude release's DOM.
// Unrecognized layouts remain inspectable (structure only), never sendable.
enum AXProfile {
    static func token(_ text: String) -> String {
        text.replacingOccurrences(of: "_", with: "-").lowercased()
    }
    static func sidebar(_ role: String, _ subrole: String, _ id: String) -> Bool {
        subrole == "AXSourceList" || subrole == "AXLandmarkNavigation" ||
        ["sidebar", "session-sidebar", "code-sidebar", "source-list", "navigation"].contains(token(id))
    }
    static func main(_ subrole: String, _ id: String) -> Bool {
        subrole == "AXLandmarkMain" ||
        ["main", "content-pane", "session-pane", "code-main", "code-content-pane", "code-session-pane"].contains(token(id))
    }
    static func header(_ role: String, _ id: String) -> Bool {
        role == "AXToolbar" ||
        ["header", "main-header", "content-header", "session-header", "project-header"].contains(token(id))
    }
    static func transcript(_ id: String) -> Bool {
        let parts = token(id).split(separator: "-")
        return parts.contains(where: {
            ["transcript", "messages", "message", "history", "conversation", "log", "markdown"].contains(String($0))
        })
    }
    static let codePanes = ["code-main", "code-content-pane", "code-session-pane"]
}

struct AXNode {
    let element: AXUIElement
    let parent: Int?
    let role: String
    let subrole: String
    let identifier: String
    let title: String?
    let sidebar: Int?
    let pane: Int?
    let header: Int?
    let explicitlyVisible: Bool
    let hidden: Bool
}

final class AXTree {
    let window: AXUIElement
    private(set) var nodes: [AXNode] = []
    private let deadline = ProcessInfo.processInfo.systemUptime + 5
    private let limit = 600

    init(window: AXUIElement) throws {
        self.window = window
        try visit(window, parent: nil, depth: 0, sidebar: nil, pane: nil,
                  header: nil, visible: true, hidden: false)
    }

    static func raw(_ element: AXUIElement, _ name: String) -> CFTypeRef? {
        guard AXCallGate.permitted else { return nil }
        var value: CFTypeRef?
        guard AXUIElementCopyAttributeValue(element, name as CFString, &value) == .success else { return nil }
        return value
    }

    static func element(_ element: AXUIElement, _ name: String) -> AXUIElement? {
        guard let value = raw(element, name), CFGetTypeID(value) == AXUIElementGetTypeID() else { return nil }
        return unsafeBitCast(value, to: AXUIElement.self)
    }

    static func string(_ element: AXUIElement, _ name: String) -> String? {
        guard let value = raw(element, name), CFGetTypeID(value) == CFStringGetTypeID() else { return nil }
        return value as? String
    }

    static func bool(_ element: AXUIElement, _ name: String) -> Bool? {
        guard let value = raw(element, name), CFGetTypeID(value) == CFBooleanGetTypeID() else { return nil }
        return (value as? NSNumber)?.boolValue
    }

    static func equal(_ a: AXUIElement, _ b: AXUIElement) -> Bool { CFEqual(a, b) }

    static func settable(_ element: AXUIElement, _ name: String) -> Bool {
        guard AXCallGate.permitted else { return false }
        var value = DarwinBoolean(false)
        return AXUIElementIsAttributeSettable(element, name as CFString, &value) == .success && value.boolValue
    }

    // Rich-text composers report an empty editor as "\n" (observed in Claude's
    // ProseMirror input), so whitespace and zero-width characters do not count as
    // content. An unreadable value is never treated as empty.
    static func composerText(_ element: AXUIElement) -> String? {
        string(element, kAXValueAttribute as String).map(normalizedComposerText)
    }

    static func composerEmpty(_ element: AXUIElement) -> Bool { composerText(element) == "" }

    static func composerHolds(_ element: AXUIElement, _ message: String) -> Bool {
        composerText(element) == normalizedComposerText(message)
    }

    static func normalizedComposerText(_ text: String) -> String {
        let visible = text.unicodeScalars.filter { !["\u{200B}", "\u{200C}", "\u{200D}", "\u{FEFF}"].contains($0) }
        return String(String.UnicodeScalarView(visible)).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    static func pressable(_ element: AXUIElement) -> Bool {
        guard AXCallGate.permitted else { return false }
        var names: CFArray?
        guard AXUIElementCopyActionNames(element, &names) == .success else { return false }
        return (names as? [String])?.contains(kAXPressAction as String) == true
    }

    private func children(_ element: AXUIElement, _ name: String) throws -> [AXUIElement]? {
        try AXCallGate.require()
        var count: CFIndex = 0
        let status = AXUIElementGetAttributeValueCount(element, name as CFString, &count)
        if status == .attributeUnsupported || status == .noValue { return nil }
        guard status == .success, count >= 0 else {
            throw BridgeFailure("ax_unavailable", "无法完整读取 AX 子结构。")
        }
        guard count <= limit else { throw BridgeFailure("traversal_limit", "AX 子结构超过安全限额。") }
        if count == 0 { return [] }
        var values: CFArray?
        guard AXUIElementCopyAttributeValues(element, name as CFString, 0, count, &values) == .success,
              let values = values as? [AXUIElement] else {
            throw BridgeFailure("ax_unavailable", "AX 子结构读取失败。")
        }
        return values
    }

    private func visit(_ element: AXUIElement, parent: Int?, depth: Int,
                       sidebar inheritedSidebar: Int?, pane inheritedPane: Int?,
                       header inheritedHeader: Int?, visible: Bool, hidden inheritedHidden: Bool) throws {
        try AXCallGate.require()
        guard ProcessInfo.processInfo.systemUptime < deadline, depth <= 24, nodes.count < limit else {
            throw BridgeFailure("traversal_limit", "AX 遍历时间、深度或数量超过安全限额。")
        }
        guard !nodes.contains(where: { Self.equal($0.element, element) }) else { return }
        guard let role = Self.string(element, kAXRoleAttribute as String) else {
            throw BridgeFailure("ax_unavailable", "AX 结构发生变化。")
        }
        let subrole = Self.string(element, kAXSubroleAttribute as String) ?? ""
        let identifier = Self.string(element, kAXIdentifierAttribute as String) ?? ""
        let hidden = inheritedHidden || Self.bool(element, "AXHidden") == true
        let index = nodes.count
        let sidebar = inheritedSidebar ?? (AXProfile.sidebar(role, subrole, identifier) ? index : nil)
        let pane = sidebar == nil ? (inheritedPane ?? (AXProfile.main(subrole, identifier) ? index : nil)) : nil
        let header = sidebar == nil && AXProfile.header(role, identifier) ? index : inheritedHeader
        let blocked = AXProfile.transcript(identifier) || (role == "AXScrollArea" && sidebar == nil)
        // No AXDescription / AXHelp / AXDocument, no values of inputs or web content.
        // Static text is read ONLY in recognized navigation / header chrome.
        let safeChrome = !hidden && !blocked && (sidebar != nil || header != nil)
        let titleRoles = ["AXStaticText", "AXHeading", "AXButton", "AXRow", "AXCell", "AXLink", "AXRadioButton", "AXTab"]
        var title: String?
        if safeChrome && titleRoles.contains(role) {
            title = Self.string(element, kAXTitleAttribute as String)
            if title == nil && role == "AXStaticText" {
                title = Self.string(element, kAXValueAttribute as String)
            }
            if let value = title, value.utf8.count > 1024 || value.contains("\n") || value.contains("\r") {
                title = nil
            }
        }
        nodes.append(AXNode(element: element, parent: parent, role: role, subrole: subrole,
                            identifier: identifier, title: title, sidebar: sidebar, pane: pane,
                            header: header, explicitlyVisible: visible, hidden: hidden))
        // Leaves and transcript regions are opaque. Do not enumerate assistant text.
        if hidden || blocked || ["AXStaticText", "AXHeading", "AXTextArea", "AXTextField", "AXSecureTextField"].contains(role) { return }
        let all = try children(element, kAXChildrenAttribute as String) ?? []
        let shown = try children(element, kAXVisibleChildrenAttribute as String)
        for child in all {
            let childVisible: Bool
            if let shown {
                childVisible = visible && shown.contains(where: { Self.equal($0, child) })
            } else if sidebar != nil && sidebar == index {
                // A virtualized sidebar must provide positive visibility evidence.
                childVisible = visible && Self.bool(child, "AXHidden") == false
            } else {
                childVisible = visible
            }
            try visit(child, parent: index, depth: depth + 1, sidebar: sidebar, pane: pane,
                      header: header, visible: childVisible, hidden: hidden)
        }
    }

    func safeNodes() -> [SafeNode] {
        let safe = nodes.indices.filter {
            let node = nodes[$0]
            return !node.hidden && node.explicitlyVisible && (node.sidebar != nil || node.header != nil) &&
                !["AXTextArea", "AXTextField", "AXSecureTextField"].contains(node.role) &&
                !AXProfile.transcript(node.identifier)
        }.prefix(120)
        let retained = Set(safe)
        return safe.map { index in
            let node = nodes[index]
            return SafeNode(index: index, parent: node.parent.flatMap { retained.contains($0) ? $0 : nil }, role: node.role,
                            subrole: node.subrole.isEmpty ? nil : String(node.subrole.prefix(128)),
                            identifier: node.identifier.isEmpty ? nil : String(node.identifier.prefix(256)),
                            title: node.title.map { String($0.prefix(128)) },
                            scope: node.sidebar != nil ? "sidebar" : "header")
        }
    }

    private func titleMatches(_ index: Int, _ title: String) -> Bool {
        if nodes[index].title == title { return true }
        return nodes.contains(where: { $0.parent == index && $0.role == "AXStaticText" && $0.title == title })
    }

    func sidebarTarget(_ binding: DesktopBinding) throws -> AXUIElement {
        let candidates = nodes.indices.filter { index in
            let node = nodes[index]
            return node.sidebar != nil && !node.hidden &&
                ["AXButton", "AXRow", "AXCell", "AXLink"].contains(node.role) && Self.pressable(node.element)
        }
        if !binding.targetTitle.isEmpty {
            let sameTitles = candidates.filter { titleMatches($0, binding.targetTitle) }
            guard sameTitles.count <= 1 else {
                throw BridgeFailure("target_ambiguous", "Sidebar 存在同名目标，拒绝自动操作。")
            }
        }
        let matches = candidates.filter { index in
            binding.targetIdentifier.isEmpty ? titleMatches(index, binding.targetTitle) :
                nodes[index].identifier == binding.targetIdentifier
        }
        guard matches.count == 1 else {
            throw BridgeFailure(matches.isEmpty ? "target_not_found" : "target_ambiguous", "Sidebar 精确 AX 目标不唯一或不存在。")
        }
        let index = matches[0]
        guard binding.targetTitle.isEmpty || titleMatches(index, binding.targetTitle) else {
            throw BridgeFailure("identity_unverified", "目标 Identifier 与绑定 Title 不一致。")
        }
        guard nodes[index].explicitlyVisible, Self.bool(nodes[index].element, kAXEnabledAttribute as String) == true else {
            throw BridgeFailure("target_not_visible", "目标没有可见且可操作的 AX 证据。")
        }
        return nodes[index].element
    }

    func identity(_ binding: DesktopBinding) throws -> Int {
        // Sidebar presence/selection is NEVER current-session evidence.
        let panes = nodes.indices.filter { index in
            let node = nodes[index]
            return node.pane == index && !node.hidden && node.explicitlyVisible &&
                (binding.targetPaneIdentifier.isEmpty || node.identifier == binding.targetPaneIdentifier)
        }
        var matches: [Int] = []
        for pane in panes {
            let chrome = nodes.indices.filter {
                let n = nodes[$0]
                return n.pane == pane && n.header != nil && n.sidebar == nil && !n.hidden && n.explicitlyVisible
            }
            let sessions = chrome.filter { index in
                let node = nodes[index]
                return binding.targetTitle.isEmpty ? node.identifier == binding.targetIdentifier : node.title == binding.targetTitle
            }
            let projects = chrome.filter { nodes[$0].title == binding.projectMarker }
            let codePane = AXProfile.codePanes.contains(AXProfile.token(nodes[pane].identifier))
            let selectedCode = nodes.filter { node in
                !node.hidden && node.explicitlyVisible && (node.sidebar != nil || node.header != nil) &&
                ["AXTab", "AXRadioButton", "AXButton"].contains(node.role) && node.title == "Code" &&
                Self.bool(node.element, kAXSelectedAttribute as String) == true
            }.count == 1
            if sessions.count == 1, projects.count == 1, sessions[0] != projects[0], codePane || selectedCode {
                matches.append(pane)
            }
        }
        guard matches.count == 1 else {
            throw BridgeFailure("identity_unverified", "缺少唯一当前内容 Pane 的会话、项目和 Code 模式证据。")
        }
        return matches[0]
    }

    func composer(in pane: Int) throws -> AXUIElement {
        let editable = nodes.filter { node in
            node.pane == pane && node.sidebar == nil && !node.hidden && node.explicitlyVisible &&
                ["AXTextArea", "AXTextField", "AXSecureTextField"].contains(node.role) &&
                Self.bool(node.element, kAXEnabledAttribute as String) == true &&
                Self.settable(node.element, kAXValueAttribute as String)
        }
        guard editable.count == 1, let node = editable.first, node.role == "AXTextArea",
              node.pane == pane, node.sidebar == nil, node.subrole != "AXSecureTextField",
              Self.bool(node.element, "AXEditable") != false else {
            throw BridgeFailure("input_ambiguous", "必须只有一个可编辑多行 AXTextArea，且位于已核验内容 Pane。")
        }
        return node.element
    }

    func sendButton(in pane: Int) throws -> AXUIElement {
        let buttons = nodes.filter { node in
            guard node.pane == pane, node.sidebar == nil, !node.hidden, node.explicitlyVisible,
                  node.role == "AXButton", Self.pressable(node.element) else { return false }
            // Read a control's explicit AXTitle, not a guessed icon/identifier/Return action.
            return Self.string(node.element, kAXTitleAttribute as String) == "Send" ||
                Self.string(node.element, kAXTitleAttribute as String) == "发送"
        }
        guard buttons.count == 1, let button = buttons.first,
              Self.bool(button.element, kAXEnabledAttribute as String) == true else {
            throw BridgeFailure("send_unavailable", "找不到唯一且明确可用的 Send/发送 AXButton。")
        }
        return button.element
    }
}
