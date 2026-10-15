import Foundation

struct TrayWindow: Decodable, Identifiable {
    let id: String?
    let name: String
    let description: String?
    let used_percent: Double?
    let blocked: Bool
    let reset_at: String?
    let limit_seconds: Int?
}
struct TrayQuotaGroup: Decodable, Identifiable {
    let id: String
    let name: String
    let scope: String
    let models: [String]?
    let windows: [TrayWindow]?
    let state: String
    let checked_at: String
    let available_at: String?
}
struct TrayQuotaMetric: Decodable { let key: String; let label: String; let value: String; let unit: String?; let currency: String? }
struct TrayResourceRef: Codable, Hashable {
    let account_id: String
    var group_id: String? = nil
    var window_id: String? = nil
}
struct TrayResourceStatus: Decodable { let resource: TrayResourceRef; let label: String; let state: String; let reason: String; let available_at: String? }
struct TrayMonitor: Decodable { let mode: String; let state: String; let resources: [TrayResourceStatus] }
struct TrayAccount: Decodable, Identifiable {
    let id: String
    let email: String
    let provider: String
    let plan: String
    let state: String
    let reason: String
    let windows: [TrayWindow]?
    let models: [String]?
    let checked_at: String
    let next_check_at: String?
    let available_at: String?
    let groups: [TrayQuotaGroup]?
    let metrics: [TrayQuotaMetric]?
    let quota_source: String?
    let quota_error: String?
    let disabled: Bool?
    let source_id: String?
}
struct TrayBinding: Decodable {
    let auto_resume: Bool
    let account_ids: [String]?
    let provider: String?
    let model: String?
    let monitor_resources: [TrayResourceRef]?
    let monitor_mode: String?
    let resume_resources: [TrayResourceRef]?
    let continue_message: String?
    let resume_owner: String?
    let native_auto_resume_disabled_acknowledged: Bool?
    let acknowledge_side_effects: Bool?
    let target_title: String?
    let target_identifier: String?
    let target_window_identifier: String?
    let target_pane_identifier: String?
    let project_marker: String?
    let deep_link: String?
    let priority: Int?
    var owner: TrayResumeOwner { TrayResumeOwner(rawValue: resume_owner ?? "") ?? .unknown }
    var guardAuthorized: Bool {
        auto_resume && owner == .guardOwner && native_auto_resume_disabled_acknowledged == true && acknowledge_side_effects == true
    }
}

enum TrayResumeOwner: String, CaseIterable, Identifiable {
    case guardOwner = "guard", native, manual, unknown
    var id: String { rawValue }
    var title: String {
        switch self {
        case .guardOwner: return "LimitBreak 负责"
        case .native: return "官方客户端负责"
        case .manual: return "用户手动恢复"
        case .unknown: return "未知（不授权）"
        }
    }
}
// Where the hook process proved the session runs; never derived from window titles.
struct TrayOrigin: Decodable {
    let app: String
    let entrypoint: String?
    let term_program: String?
    let bundle_id: String?
    let host_session_id: String?
    let claude_pid: Int?
    let ancestors: [Int]?
    let engine_version: String?
    let exec_path: String?
    let tmux_pane: String?
    let detached: Bool?
    let observed_at: String?
    let client: String?
    var appTitle: String {
        if app == "client" { return "客户端 " + (client ?? "?") }
        return ["claude-desktop": "Claude 桌面 Code", "terminal": "终端", "unknown": "未知来源"][app] ?? "未知来源"
    }
}
struct TrayPool: Decodable {
    let model: String?
    let provider: String?
    let account_ids: [String]?
    let basis: String?
    let inferred: Bool?
    var basisTitle: String {
        ["manual": "手动指定", "alias": "模型映射", "exact": "模型精确匹配", "prefix": "模型前缀推断",
         "provider": "按供应商推断", "none": "未匹配"][basis ?? ""] ?? (basis ?? "未知")
    }
}
struct TrayHost: Decodable {
    let mode: String
    let executor: String?
    let target: String?
    let ready: Bool
    let problem: String?
    var executorTitle: String {
        if let executor, executor.hasPrefix("client:") { return "客户端命令 " + executor.dropFirst(7) }
        return ["claude-desktop": "Claude 桌面", "claude-terminal": "终端 claude"][executor ?? ""] ?? "未定位"
    }
}
struct TraySession: Decodable, Identifiable {
    let id: String
    let title: String
    let cwd: String
    let model: String
    let binding: TrayBinding?
    let state: String
    let reason: String
    let kind: String?
    let monitor: TrayMonitor?
    let generation: Int64?
    let provider: String?
    let error: String?
    let updated_at: String?
    let origin: TrayOrigin?
    let host_mode: String?
    let continue_message: String?
    let pool_override: [String]?
    let target_override: String?
    let native_resume: String?
    let priority: Int?
    let permission_mode: String?
    let pool: TrayPool?
    let host: TrayHost?
    let reason_text: String?
    let in_catalog: Bool?
    let archived: Bool?
    let last_activity_at: String?
    var displayTitle: String {
        if !title.isEmpty { return title }
        let folder = (cwd as NSString).lastPathComponent
        return folder.isEmpty ? "未命名会话" : folder
    }
    var explanation: String { reason_text?.isEmpty == false ? reason_text! : reason }
    var isArchived: Bool { archived == true }
    var projectName: String { cwd.isEmpty ? "" : (cwd as NSString).lastPathComponent }
}
struct TrayJob: Decodable {
    let id: String?
    let session_id: String
    let state: String
    let not_before: String
    let kind: String?
    let reason: String?
    let reason_text: String?
    let account_id: String?
    let attempts: Int?
    let message: String?
    let reset_hint: String?
    let generation: Int64?
    let created_at: String?
    let updated_at: String?
    let delivery: String?
    var stableID: String { id ?? session_id + "/" + not_before }
    var active: Bool { !["COMPLETED", "CANCELLED"].contains(state) }
}
struct TrayAudit: Decodable, Identifiable {
    let id: Int64
    let time: String
    let kind: String
    let session_id: String
    let message: String
    let text: String?
    var display: String { text?.isEmpty == false ? text! : message }
}
struct TraySnapshot: Decodable {
    let accounts: [TrayAccount]?
    let sessions: [TraySession]?
    let jobs: [TrayJob]?
    let audit: [TrayAudit]?
    let adapters: [GuardAdapter]?
    let sources: [GuardSource]?
    let signals: [TraySignalSource]?
    let policy: [String: JSONValue]
    let capabilities: [String: JSONValue]
    let policy_digest: String?
    var paused: Bool { policy["paused"]?.bool ?? true }
    var automationConsented: Bool { policy["automation_consent_at"]?.string?.isEmpty == false }
    var defaultHostMode: String { policy["default_host_mode"]?.string ?? "" }
    var modelAliases: [String: String] {
        guard case .object(let map)? = policy["model_aliases"] else { return [:] }
        return map.compactMapValues(\.string)
    }
}

// Reset signals only make quota be queried sooner; they are never evidence that quota recovered.
struct TrayResetSignal: Decodable {
    let id: String
    let provider: String
    let kind: String
    let at: String
    let text: String?
    let url: String?
}
struct TrayScheduledReset: Decodable { let id: String; let at: String?; let text: String? }
struct TrayResetWatch: Decodable {
    let level: String
    let chance_percent: Int?
    let window: String?
    let text: String?
}
struct TraySignalSource: Decodable, Identifiable {
    let id: String
    let type: String
    let name: String
    let provider: String
    let enabled: Bool
    let state: String
    let error: String?
    let last_poll_at: String?
    let next_poll_at: String?
    let latest: TrayResetSignal?
    let scheduled: TrayScheduledReset?
    let watch: TrayResetWatch?
    let last_triggered_at: String?
    let attribution: String?
    let attribution_url: String?
}

// Step 1 of setup: waiting for the first quota read is shown as progress, not as "not connected".
enum TraySourcePhase { case unconfigured, starting, checking, connected, failed }

enum TrayHostMode: String, CaseIterable, Identifiable {
    case follow = "", hosted, monitor, off
    var id: String { rawValue }
    var title: String {
        switch self {
        case .follow: return "跟随全局"
        case .hosted: return "智能托管（自动继续）"
        case .monitor: return "仅监控"
        case .off: return "关闭"
        }
    }
    static func title(_ raw: String) -> String {
        raw == "legacy" ? "旧版绑定" : (TrayHostMode(rawValue: raw)?.title ?? raw)
    }
}

// Keeps the full policy intact when the simple menu changes only its pause switch.
enum JSONValue: Codable, Equatable {
    case string(String), number(Double), bool(Bool), object([String: JSONValue]), array([JSONValue]), null
    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null }
        else if let v = try? c.decode(Bool.self) { self = .bool(v) }
        else if let v = try? c.decode(Double.self) { self = .number(v) }
        else if let v = try? c.decode(String.self) { self = .string(v) }
        else if let v = try? c.decode([String: JSONValue].self) { self = .object(v) }
        else { self = .array(try c.decode([JSONValue].self)) }
    }
    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let v): try c.encode(v)
        case .number(let v): try c.encode(v)
        case .bool(let v): try c.encode(v)
        case .object(let v): try c.encode(v)
        case .array(let v): try c.encode(v)
        case .null: try c.encodeNil()
        }
    }
    var bool: Bool? { if case .bool(let v) = self { return v }; return nil }
    var string: String? { if case .string(let v) = self { return v }; return nil }
    var number: Double? { if case .number(let v) = self { return v }; return nil }
}

// Building a formatter costs far more than parsing; render paths call this per row every second.
private enum TrayDateParsers {
    static let fractional: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter
    }()
    static let plain = ISO8601DateFormatter()
}
func trayDate(_ text: String?) -> Date? {
    guard let text, !text.isEmpty else { return nil }
    return TrayDateParsers.fractional.date(from: text) ?? TrayDateParsers.plain.date(from: text)
}
func trayDuration(_ seconds: TimeInterval) -> String {
    let s = max(0, Int(seconds))
    if s >= 86400 { return "\(s / 86400)天 " + String(format: "%02d:%02d", s % 86400 / 3600, s % 3600 / 60) }
    return String(format: "%02d:%02d:%02d", s / 3600, s % 3600 / 60, s % 60)
}
func trayState(_ state: String) -> String {
    ["RUNNING": "运行中", "COOLDOWN": "等待额度恢复", "READY_TO_RESUME": "等待恢复",
     "VERIFYING": "正在核验恢复", "DISPATCHING": "正在恢复", "COMPLETED": "已完成",
     "CANCELLED": "已停止", "NEEDS_ATTENTION": "需要处理", "allowed": "额度可用",
     "limited": "额度受限", "unknown": "额度未知", "disabled": "已停用", "stale": "等待复查", "missing": "资源未取得"][state] ?? state
}
