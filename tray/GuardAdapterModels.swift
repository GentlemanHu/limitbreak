import Foundation

struct GuardAdapter: Decodable, Identifiable {
    let id: String
    let name: String
    let kind: String
    let implemented: Bool
    let read_only: Bool
    let capabilities: [String]?
    let limitations: [String]?
    var kindTitle: String { ["quota_source": "额度来源", "session_observer": "会话观察", "resume_executor": "恢复执行", "reset_signal": "重置信号"][kind] ?? kind }
}
struct GuardSource: Decodable, Identifiable {
    let id: String
    let type: String
    let state: String
    let connected: Bool
    let checked_at: String
    let last_success_at: String?
    let error: String?
    let account_count: Int
}
struct GuardAdapterCatalog: Decodable { let adapters: [GuardAdapter] }
struct GuardSourceCatalog: Decodable { let sources: [GuardSource] }
