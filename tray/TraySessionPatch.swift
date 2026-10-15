import Foundation

// Partial per-session settings; a nil field is left untouched by the server.
struct TraySessionPatch: Encodable, Equatable {
    var host_mode: String?
    var continue_message: String?
    var pool_override: [String]?
    var target_override: String?
    var priority: Int?
    var isEmpty: Bool { self == TraySessionPatch() }

    // Mirrors domain.ValidateSessionSettings so the user sees the reason before a round trip.
    var problem: String? {
        if let host_mode, TrayHostMode(rawValue: host_mode) == nil { return "托管模式无效" }
        if let text = continue_message, !text.isEmpty,
           text.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || text.utf8.count > 4096 || text.contains("\n") || text.contains("\r") || text.contains("\0") {
            return "继续文案须为单行、非空、≤4096 字节；留空表示跟随全局"
        }
        if let target = target_override, !Self.validTarget(target) { return "发送目标只能是跟随来源或 Claude 桌面" }
        if let priority, !(-100...100).contains(priority) { return "优先级须在 -100 至 100 之间" }
        if let pool = pool_override, pool.count > 64 || pool.contains(where: { $0.isEmpty || $0.utf8.count > 512 }) { return "手动额度池最多 64 个账号" }
        return nil
    }
    static func validTarget(_ value: String) -> Bool { value.isEmpty || value == "desktop" }
}
struct TraySessionPatchResponse: Decodable { let ok: Bool; let session: TraySession }
struct TrayLocateResult: Decodable { let ok: Bool; let code: String?; let message: String? }
struct TrayLocateResponse: Decodable { let ok: Bool; let result: TrayLocateResult }
struct TrayTestSendResponse: Decodable { let ok: Bool; let job_id: String?; let state: String? }
