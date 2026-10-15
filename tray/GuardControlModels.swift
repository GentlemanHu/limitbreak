import Foundation

struct GuardMCPGrant: Decodable, Identifiable {
    let id: String
    let scopes: [String]
    let session_targets: [String]
    let created_at: String
    let expires_at: String
    let revoked_at: String?
    func active(at now: Date) -> Bool { revoked_at == nil && trayDate(created_at).map { $0 <= now } == true && trayDate(expires_at).map { $0 > now } == true }
}
struct GuardMCPGrantPage: Decodable { let grants: [GuardMCPGrant]; let next_cursor: String? }
struct GuardMCPIssuedGrant: Decodable { let grant: GuardMCPGrant; let access_token: String }
struct GuardMCPAction: Decodable {
    let operation: String
    let session_id: String
    let expected_generation: Int64
    let policy_digest: String?
    let params: [String: JSONValue]
    var supported: Bool {
        guard GuardMCPContract.operations.contains(operation), GuardMCPContract.validID(session_id), expected_generation >= 0 else { return false }
        let global = ["refresh", "pause", "unpause", "settingsPatch"].contains(operation)
        guard global == (session_id == "daemon"), !global || expected_generation == 0 else { return false }
        let allowed: Set<String>
        switch operation {
        case "bind_monitor": allowed = ["monitor_resources", "monitor_mode"]
        case "enable_auto_resume": allowed = ["enabled"]
        case "settingsPatch": allowed = ["settings_patch"]
        case "session_settings": allowed = ["session_settings"]
        case "test_send": allowed = ["message"]
        default: allowed = []
        }
        guard Set(params.keys).isSubset(of: allowed) else { return false }
        if ["pause", "unpause", "settingsPatch"].contains(operation) && !GuardMCPContract.digest(policy_digest) { return false }
        if operation == "enable_auto_resume" && params["enabled"]?.bool == nil { return false }
        if operation == "bind_monitor" {
            guard let mode = params["monitor_mode"]?.string, ["any", "all"].contains(mode),
                  case .array(let refs)? = params["monitor_resources"], !refs.isEmpty, refs.count <= 64 else { return false }
            for ref in refs {
                guard case .object(let object) = ref, Set(object.keys).isSubset(of: ["account_id", "group_id", "window_id"]),
                      let id = object["account_id"]?.string, GuardMCPContract.validResourceID(id), object.values.allSatisfy({ value in
                          guard let string = value.string else { return false }
                          return string.isEmpty || GuardMCPContract.validResourceID(string)
                      }) else { return false }
            }
        }
        if operation == "session_settings" {
            guard case .object(let settings)? = params["session_settings"], !settings.isEmpty,
                  Set(settings.keys).isSubset(of: ["host_mode", "continue_message", "pool_override", "target_override", "priority"]) else { return false }
        }
        if operation == "test_send", let message = params["message"] {
            guard let text = message.string, !text.isEmpty, text.utf8.count <= 4096, !text.contains("\n"), !text.contains("\r") else { return false }
        }
        if operation == "settingsPatch" {
            guard case .object(let patch)? = params["settings_patch"], !patch.isEmpty, patch.count <= 16,
                  Set(patch.keys).isSubset(of: GuardPolicyPatch.controlNumericKeys), patch.values.allSatisfy({ value in
                      guard let number = value.number else { return false }
                      return number.isFinite && number.rounded() == number && (0...86400).contains(number)
                  }) else { return false }
        }
        return true
    }
    var exactParameters: String {
        let encoder = JSONEncoder(); encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        guard let data = try? encoder.encode(params), let text = String(data: data, encoding: .utf8) else { return "参数不可读取；禁止批准" }
        return text
    }
}
struct GuardMCPResult: Decodable {
    let ok: Bool
    let code: String
    let generation: Int64?
    let send_retraction_confirmed: Bool
}
struct GuardMCPRequest: Decodable, Identifiable {
    let id: String
    let grant_id: String
    let action: GuardMCPAction
    let digest: String
    let state: String
    let created_at: String
    let expires_at: String
    let result: GuardMCPResult?
    func confirmable(at now: Date) -> Bool {
        state == "pending" && action.supported && GuardMCPContract.validID(id) && GuardMCPContract.digest(digest)
            && trayDate(created_at).map { $0 <= now } == true && trayDate(expires_at).map { $0 > now } == true
    }
}
struct GuardMCPRequestPage: Decodable { let requests: [GuardMCPRequest]; let next_cursor: String? }
struct GuardMCPRequestResponse: Decodable { let request: GuardMCPRequest }

enum GuardMCPContract {
    static let operations = ["refresh", "cancel", "pause", "unpause", "bind_monitor", "enable_auto_resume", "settingsPatch", "session_settings", "test_send", "locate"]
    static let scopes = ["read"] + operations.map { "action:" + $0 }
    static func digest(_ value: String?) -> Bool {
        guard let value else { return false }
        return value.utf8.count == 64 && value.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
    }
    static func validID(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.count <= 256 && value.utf8.allSatisfy { (33...126).contains($0) && ![UInt8(47), 92, 34].contains($0) }
    }
    static func validResourceID(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.count <= 512 && value == value.trimmingCharacters(in: .whitespacesAndNewlines) && !value.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) })
    }
    static func createGrant(scopes chosenScopes: Set<String>, targets: Set<String>, ttl: Int) throws -> Data {
        guard chosenScopes.contains("read"), chosenScopes.isSubset(of: Set(scopes)), !targets.isEmpty, targets.count <= 128,
              targets.allSatisfy(validID), (60...86400).contains(ttl) else { throw RuntimeFailure("授权必须包含只读scope、明确目标和60至86400秒有效期；不接受通配符") }
        guard !targets.contains("*") else { throw RuntimeFailure("授权不能包含所有会话通配符") }
        return try JSONSerialization.data(withJSONObject: ["scopes": chosenScopes.sorted(), "session_targets": targets.sorted(), "ttl_seconds": ttl])
    }
    static func decision(_ request: GuardMCPRequest, approve: Bool, now: Date) throws -> Data {
        guard request.confirmable(at: now) else { throw RuntimeFailure("请求已变化、过期或参数未知；禁止批准或重复执行") }
        return try JSONSerialization.data(withJSONObject: ["request_id": request.id, "digest": request.digest, "expected_generation": request.action.expected_generation, "decision": approve ? "approve" : "reject"])
    }
}
