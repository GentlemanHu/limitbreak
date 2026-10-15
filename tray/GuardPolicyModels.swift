import Foundation

struct GuardPolicyPatchResponse: Decodable {
    let ok: Bool
    let policy: [String: JSONValue]
    let policy_digest: String
}

enum GuardPolicyPatch {
    // MCP settingsPatch accepts exactly these timing/limit keys.
    static let controlNumericKeys: Set<String> = ["max_running", "max_per_account", "safety_seconds", "freshness_seconds", "verify_seconds", "long_probe_seconds", "unknown_probe_seconds", "min_send_interval_seconds", "early_probe_seconds", "near_reset_probe_seconds", "reset_lookahead_seconds", "user_idle_seconds", "running_stale_seconds", "queue_aging_seconds", "max_auto_resumes_per_hour"]
    static let numericKeys: Set<String> = controlNumericKeys.union(["native_grace_seconds", "claude_quiet_seconds"])
    static let otherKeys: Set<String> = ["paused", "continue_message", "default_host_mode", "automation_consent", "model_aliases", "direct_delivery", "reset_fallback", "restore_focus", "claude_quiet"]

    static func normalizeModel(_ model: String) -> String {
        var m = model.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        if let i = m.firstIndex(of: "["), i > m.startIndex { m = String(m[..<i]) }
        return m.trimmingCharacters(in: .whitespacesAndNewlines)
    }
    static func validAlias(from: String, to: String) -> Bool {
        let plain = { (s: String) in !s.isEmpty && s.utf8.count <= 128 && !s.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) }
        return plain(from) && plain(to) && normalizeModel(from) == from
    }

    static func data(_ changes: [String: JSONValue], digest: String?) throws -> Data {
        guard let digest, digest.utf8.count == 64, digest.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) }),
              !changes.isEmpty, Set(changes.keys).isSubset(of: numericKeys.union(otherKeys)) else { throw RuntimeFailure("策略摘要未知或修改字段无效，请重新读取并确认") }
        for (key, value) in changes {
            switch key {
            case _ where numericKeys.contains(key):
                guard let number = value.number, number.isFinite, number.rounded() == number, (0...86400).contains(number),
                      key != "user_idle_seconds" || (1...3600).contains(number), key != "native_grace_seconds" || (1...3600).contains(number),
                      key != "claude_quiet_seconds" || (1...3600).contains(number) else { throw RuntimeFailure("策略数值无效") }
            case "paused", "automation_consent":
                guard value.bool != nil else { throw RuntimeFailure("开关值必须为布尔值") }
            case "default_host_mode":
                guard let mode = value.string, TrayHostMode(rawValue: mode) != nil else { throw RuntimeFailure("默认托管模式无效") }
            case "direct_delivery":
                guard let mode = value.string, mode == "auto" || mode == "off" else { throw RuntimeFailure("后台直发设置只能为 auto 或 off") }
            case "reset_fallback":
                guard let mode = value.string, mode == "auto" || mode == "off" else { throw RuntimeFailure("重置后尝试继续的设置只能为 auto 或 off") }
            case "restore_focus":
                guard let mode = value.string, mode == "auto" || mode == "off" else { throw RuntimeFailure("还原焦点的设置只能为 auto 或 off") }
            case "claude_quiet":
                guard let mode = value.string, mode == "auto" || mode == "off" else { throw RuntimeFailure("避开正在使用的 Claude 的设置只能为 auto 或 off") }
            case "model_aliases":
                guard case .object(let map) = value, map.count <= 64,
                      map.allSatisfy({ validAlias(from: $0.key, to: $0.value.string ?? "") }) else { throw RuntimeFailure("模型映射无效：最多64条，左侧须为小写规范模型名") }
            default:
                guard let text = value.string, !text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty, text.utf8.count <= 4096,
                      !text.contains("\0"), !text.contains("\n"), !text.contains("\r") else { throw RuntimeFailure("默认继续文案无效（须为单行、非空、≤4096字节）") }
            }
        }
        return try JSONEncoder().encode(["policy_digest": JSONValue.string(digest), "patch": .object(changes)])
    }

    // The server omits empty/false optional fields, and consent is stored as a timestamp.
    static func confirms(_ changes: [String: JSONValue], policy: [String: JSONValue]) -> Bool {
        changes.allSatisfy { key, value in
            switch key {
            case "automation_consent":
                let granted = policy["automation_consent_at"]?.string?.isEmpty == false
                return value.bool == granted
            case "default_host_mode":
                return (policy[key]?.string ?? "") == value.string
            case "direct_delivery", "reset_fallback", "restore_focus", "claude_quiet":
                // An unset value means the default, "auto".
                let stored = policy[key]?.string ?? ""
                return (stored.isEmpty ? "auto" : stored) == value.string
            case "model_aliases":
                guard case .object(let wanted) = value else { return false }
                if case .object(let got)? = policy[key] { return got == wanted }
                return wanted.isEmpty
            default:
                return policy[key] == value
            }
        }
    }
}
