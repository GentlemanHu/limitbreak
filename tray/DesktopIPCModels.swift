import CryptoKit
import Foundation

struct DesktopIPCRequest: Decodable {
    let `protocol`: Int
    let request_id: String
    let token: String
    let operation: String
    let generation: Int64?
    let lease_id: String?
    let message_hash: String?
    let binding: DesktopBinding?
    let message: String?
    let user_idle_seconds: Int?
    let quiet_seconds: Int?
    let restore_focus: Bool?
    let deadline: String
    var deadlineDate: Date { trayDate(deadline) ?? .distantPast }
    static let keys: Set<String> = ["protocol", "request_id", "token", "operation", "generation", "lease_id", "message_hash", "binding", "message", "user_idle_seconds", "quiet_seconds", "restore_focus", "deadline"]
    static let bindingKeys: Set<String> = [
        "session_id", "kind", "target_title", "target_identifier", "target_window_identifier", "target_pane_identifier",
        "project_marker", "deep_link", "auto_resume", "acknowledge_side_effects", "resume_owner",
        "native_auto_resume_disabled_acknowledged", "continue_message", "provider", "model", "account_ids", "priority",
        "monitor_resources", "monitor_mode", "resume_resources", "executor",
    ]
    static func isHex(_ text: String, count: Int) -> Bool {
        text.utf8.count == count && text.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
    }
    static func equalSecret(_ a: String, _ b: String) -> Bool {
        let left = Array(a.utf8), right = Array(b.utf8)
        guard left.count == right.count else { return false }
        return zip(left, right).reduce(UInt8(0)) { $0 | ($1.0 ^ $1.1) } == 0
    }
    static func decode(_ data: Data, expectedToken: String, now: Date = Date()) throws -> DesktopIPCRequest {
        guard data.count <= 131072, !data.contains(0x0A), !data.contains(0x0D) else { throw invalid() }
        try StrictIPCJSON.check(data)
        guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any], Set(object.keys).isSubset(of: keys) else { throw invalid() }
        let request = try JSONDecoder().decode(Self.self, from: data)
        guard request.protocol == 1, isHex(request.request_id, count: 32), isHex(request.token, count: 64),
              equalSecret(request.token, expectedToken), ["probe", "inspect", "validate_target", "locate", "wake", "resume"].contains(request.operation),
              request.deadline.range(of: #"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?(Z|[+-]\d{2}:\d{2})$"#, options: .regularExpression) != nil,
              request.deadlineDate > now, request.deadlineDate.timeIntervalSince(now) <= 45 else { throw invalid() }
        if let idle = request.user_idle_seconds, !(1...3600).contains(idle) { throw invalid() }
        if let quiet = request.quiet_seconds, !(0...3600).contains(quiet) { throw invalid() }
        if let rawBinding = object["binding"] {
            guard let value = rawBinding as? [String: Any], Set(value.keys).isSubset(of: bindingKeys) else { throw invalid() }
            for key in ["monitor_resources", "resume_resources"] {
                if let refs = value[key] {
                    guard let refs = refs as? [[String: Any]], refs.count <= 256 else { throw invalid() }
                    for ref in refs {
                        guard Set(ref.keys).isSubset(of: ["account_id", "group_id", "window_id"]), let account = ref["account_id"] as? String,
                              !account.isEmpty, ref.values.allSatisfy({ value in
                                  guard let text = value as? String else { return false }
                                  return text.utf8.count <= 4096 && !text.contains("\0")
                              }) else { throw invalid() }
                    }
                }
            }
            try request.binding?.validateIdentity()
        }
        switch request.operation {
        case "probe", "inspect":
            guard request.binding == nil, request.message == nil, request.generation == nil, request.lease_id == nil, request.message_hash == nil else { throw invalid() }
        case "validate_target":
            guard request.binding != nil, request.message == nil, request.generation == nil, request.lease_id == nil, request.message_hash == nil else { throw invalid() }
        case "locate":
            // Locate only opens the session's own link; it never types.
            guard let binding = request.binding, binding.isDeepLinkTarget, request.message == nil, request.generation == nil,
                  request.lease_id == nil, request.message_hash == nil else { throw invalid() }
        case "wake":
            // Wake opens the session's link without typing, under the same lease as the send it precedes.
            guard let binding = request.binding, binding.isDeepLinkTarget, request.message == nil, validLease(request) else { throw invalid() }
        case "resume":
            guard let binding = request.binding, let message = request.message, validLease(request),
                  let hash = request.message_hash else { throw invalid() }
            let actual = SHA256.hash(data: Data(message.utf8)).map { String(format: "%02x", $0) }.joined()
            guard equalSecret(actual, hash) else { throw invalid() }
            try ResumeRequest(binding: binding, message: message).validate()
        default: throw invalid()
        }
        return request
    }
    static func validLease(_ request: DesktopIPCRequest) -> Bool {
        guard let generation = request.generation, generation > 0, let lease = request.lease_id, !lease.isEmpty, lease.utf8.count <= 512,
              lease.utf8.allSatisfy({ (33...126).contains($0) }), let hash = request.message_hash, isHex(hash, count: 64) else { return false }
        return true
    }
    static func invalid() -> BridgeFailure { BridgeFailure("invalid_request", "IPC字段、认证、白名单或有效期限校验失败。") }
}
struct DesktopIPCResponse: Encodable {
    let `protocol` = 1
    let request_id: String
    let result: BridgeResult
    var inspection: Inspection? = nil
}

// JSONDecoder accepts duplicate keys. Reject them before decoding an authenticated contract.
enum StrictIPCJSON {
    static func check(_ data: Data) throws {
        var parser = Parser(bytes: Array(data))
        try parser.value(depth: 0)
        parser.space()
        guard parser.index == parser.bytes.count else { throw DesktopIPCRequest.invalid() }
    }
    private struct Parser {
        let bytes: [UInt8]
        var index = 0
        mutating func space() { while index < bytes.count && [UInt8(32), 9].contains(bytes[index]) { index += 1 } }
        mutating func consume(_ byte: UInt8) throws {
            space(); guard index < bytes.count, bytes[index] == byte else { throw DesktopIPCRequest.invalid() }; index += 1
        }
        mutating func string() throws -> String {
            space(); let start = index; try consume(34)
            var escaped = false
            while index < bytes.count {
                let byte = bytes[index]; index += 1
                if escaped { escaped = false; continue }
                if byte == 92 { escaped = true; continue }
                if byte == 34 {
                    guard let text = try JSONSerialization.jsonObject(with: Data(bytes[start..<index]), options: .fragmentsAllowed) as? String else { throw DesktopIPCRequest.invalid() }
                    return text
                }
                if byte < 32 { throw DesktopIPCRequest.invalid() }
            }
            throw DesktopIPCRequest.invalid()
        }
        mutating func value(depth: Int) throws {
            space(); guard depth <= 12, index < bytes.count else { throw DesktopIPCRequest.invalid() }
            switch bytes[index] {
            case 123:
                index += 1; space(); var seen = Set<String>()
                if index < bytes.count && bytes[index] == 125 { index += 1; return }
                while true {
                    let key = try string(); guard seen.insert(key).inserted else { throw DesktopIPCRequest.invalid() }
                    try consume(58); try value(depth: depth + 1); space()
                    guard index < bytes.count else { throw DesktopIPCRequest.invalid() }
                    if bytes[index] == 125 { index += 1; return }; try consume(44)
                }
            case 91:
                index += 1; space()
                if index < bytes.count && bytes[index] == 93 { index += 1; return }
                while true {
                    try value(depth: depth + 1); space(); guard index < bytes.count else { throw DesktopIPCRequest.invalid() }
                    if bytes[index] == 93 { index += 1; return }; try consume(44)
                }
            case 34: _ = try string()
            default:
                let start = index
                while index < bytes.count && ![UInt8(32), 9, 44, 93, 125].contains(bytes[index]) { index += 1 }
                guard index > start else { throw DesktopIPCRequest.invalid() }
                _ = try JSONSerialization.jsonObject(with: Data(bytes[start..<index]), options: .fragmentsAllowed)
            }
        }
    }
}
