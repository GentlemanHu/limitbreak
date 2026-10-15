import Foundation

// Wire names mirror the daemon domain. Errors never contain input or raw AX text.
struct BridgeResult: Codable {
    var ok = false
    var submitted = false
    var code: String
    var message: String
    var accessibilityGranted = false
    var screenLocked = true
    enum CodingKeys: String, CodingKey {
        case ok, submitted, code, message
        case accessibilityGranted = "accessibility_granted"
        case screenLocked = "screen_locked"
    }
}

struct DesktopBinding: Decodable {
    let sessionID: String
    let kind: String
    let targetTitle: String
    let targetIdentifier: String
    let targetWindowIdentifier: String
    let targetPaneIdentifier: String
    let projectMarker: String
    let deepLink: String
    let autoResume: Bool
    let acknowledgeSideEffects: Bool
    let resumeOwner: String
    let nativeAutoResumeDisabledAcknowledged: Bool
    let continueMessage: String
    let executor: String
    static let desktopExecutor = "claude-desktop"
    static let deepLinkPrefix = "claude://claude.ai/epitaxy/"
    static func validDeepLink(_ link: String) -> Bool {
        link.range(of: #"^claude://claude\.ai/epitaxy/local_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"#,
                   options: .regularExpression) != nil
    }
    // A hosted desktop session is addressed by its own link; the daemon's
    // generation/lease check is the authority for whether it may be written.
    var isDeepLinkTarget: Bool { kind == "code" && executor == Self.desktopExecutor && Self.validDeepLink(deepLink) }
    var hostSessionID: String { isDeepLinkTarget ? String(deepLink.dropFirst(Self.deepLinkPrefix.count)) : "" }
    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id", kind, executor
        case targetTitle = "target_title", targetIdentifier = "target_identifier"
        case targetWindowIdentifier = "target_window_identifier", targetPaneIdentifier = "target_pane_identifier"
        case projectMarker = "project_marker", deepLink = "deep_link"
        case autoResume = "auto_resume", acknowledgeSideEffects = "acknowledge_side_effects"
        case resumeOwner = "resume_owner", continueMessage = "continue_message"
        case nativeAutoResumeDisabledAcknowledged = "native_auto_resume_disabled_acknowledged"
    }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        sessionID = try c.decode(String.self, forKey: .sessionID)
        kind = try c.decode(String.self, forKey: .kind)
        targetTitle = try c.decodeIfPresent(String.self, forKey: .targetTitle) ?? ""
        targetIdentifier = try c.decodeIfPresent(String.self, forKey: .targetIdentifier) ?? ""
        targetWindowIdentifier = try c.decodeIfPresent(String.self, forKey: .targetWindowIdentifier) ?? ""
        targetPaneIdentifier = try c.decodeIfPresent(String.self, forKey: .targetPaneIdentifier) ?? ""
        projectMarker = try c.decodeIfPresent(String.self, forKey: .projectMarker) ?? ""
        deepLink = try c.decodeIfPresent(String.self, forKey: .deepLink) ?? ""
        autoResume = try c.decodeIfPresent(Bool.self, forKey: .autoResume) ?? false
        acknowledgeSideEffects = try c.decodeIfPresent(Bool.self, forKey: .acknowledgeSideEffects) ?? false
        resumeOwner = try c.decodeIfPresent(String.self, forKey: .resumeOwner) ?? ""
        nativeAutoResumeDisabledAcknowledged = try c.decodeIfPresent(Bool.self, forKey: .nativeAutoResumeDisabledAcknowledged) ?? false
        continueMessage = try c.decodeIfPresent(String.self, forKey: .continueMessage) ?? ""
        executor = try c.decodeIfPresent(String.self, forKey: .executor) ?? ""
    }
    func validateIdentity() throws {
        guard kind == "code" else { throw BridgeFailure("unsupported", "仅支持 Code；Chat/Cowork 不支持恢复。") }
        guard executor.isEmpty || executor == Self.desktopExecutor else {
            throw BridgeFailure("executor_unsupported", "该绑定不属于桌面执行器。")
        }
        // Legacy guard bindings never follow deep_link; only the executor form does.
        guard !executor.isEmpty == isDeepLinkTarget else {
            throw BridgeFailure("invalid_binding", "桌面托管绑定需要合法会话深链接。")
        }
        guard !sessionID.isEmpty, isDeepLinkTarget || (!projectMarker.isEmpty && (!targetTitle.isEmpty || !targetIdentifier.isEmpty)) else {
            throw BridgeFailure("invalid_binding", "需要会话、精确目标和非空项目标识。")
        }
        for text in [sessionID, targetTitle, targetIdentifier, targetWindowIdentifier, targetPaneIdentifier, projectMarker] {
            guard text.utf8.count <= 4096, text == text.trimmingCharacters(in: .whitespacesAndNewlines),
                  !text.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else {
                throw BridgeFailure("invalid_binding", "绑定标识格式不合法。")
            }
        }
        guard ["", "guard", "auto", "native", "manual", "unknown"].contains(resumeOwner), continueMessage.utf8.count <= 4096,
              !continueMessage.contains("\0"), deepLink.utf8.count <= 4096, !deepLink.contains("\0") else {
            throw BridgeFailure("invalid_binding", "恢复负责人或继续文案格式不合法。")
        }
    }
}

struct ResumeRequest: Decodable {
    let binding: DesktopBinding
    let message: String
    func validate() throws {
        try binding.validateIdentity()
        if !binding.isDeepLinkTarget {
            guard binding.autoResume, binding.acknowledgeSideEffects else {
                throw BridgeFailure("consent_required", "绑定须启用自动恢复并明确确认副作用。")
            }
            guard binding.resumeOwner == "guard", binding.nativeAutoResumeDisabledAcknowledged else {
                throw BridgeFailure("resume_owner_blocked", "仅守护负责且明确确认官方自动继续已禁用的会话可发送。")
            }
        } else if ["native", "manual"].contains(binding.resumeOwner) {
            throw BridgeFailure("resume_owner_blocked", "该会话由原生或手动继续负责。")
        }
        guard !message.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, message.utf8.count <= 4096,
              !message.contains("\0"), binding.continueMessage.isEmpty || binding.continueMessage == message else {
            throw BridgeFailure("invalid_message", "继续文案为空、过大、包含 NUL 或与绑定不一致。")
        }
    }
}

struct BridgeFailure: Error {
    let code: String
    let message: String
    init(_ code: String, _ message: String) { self.code = code; self.message = message }
}
struct Inspection: Codable {
    let result: BridgeResult
    let nodes: [SafeNode]
    let profile: String
}
struct SafeNode: Codable {
    let index: Int
    let parent: Int?
    let role: String
    let subrole: String?
    let identifier: String?
    let title: String?
    let scope: String
}
