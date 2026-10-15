import Foundation

enum GuardPermissionState: String {
    case unknown, granted, denied
    var title: String {
        switch self {
        case .unknown: return "未知／尚未检查"
        case .granted: return "已授权（当前主应用）"
        case .denied: return "未授权／系统拒绝（具体 TCC 原因未知）"
        }
    }
}
struct GuardPermissionObservation {
    var state: GuardPermissionState = .unknown
    var checkedAt: Date?
    var requestedAt: Date?
}
struct GuardDesktopProbeResponse: Decodable {
    let ok: Bool
    let status: String
    let result: [String: JSONValue]
    var completed: Bool { ok && status == "completed" }
}
struct GuardDesktopObservation {
    let checkedAt: Date?
    let checked: Bool
    let permission: GuardPermissionState
    let bridgeAvailable: Bool
    let screenLocked: Bool?
    let code: String
    init(_ capabilities: [String: JSONValue], now: Date = Date()) {
        let time = trayDate(capabilities["desktop_checked_at"]?.string)
        checkedAt = time.flatMap { $0 <= now && $0.timeIntervalSince1970 > 0 ? $0 : nil }
        checked = capabilities["bridge_checked"]?.bool == true && checkedAt != nil
        code = capabilities["desktop_code"]?.string ?? ""
        bridgeAvailable = checked && capabilities["bridge_available"]?.bool == true
        if checked && capabilities["accessibility_state"]?.string == "granted" && capabilities["accessibility_granted"]?.bool == true { permission = .granted }
        else if checked && capabilities["accessibility_state"]?.string == "denied" { permission = .denied }
        else { permission = .unknown }
        let unknownLockCodes = ["environment_unverified", "console_inactive", "lock_state_unverified", "cancelled", "bridge_unavailable", "ipc_unavailable", "ipc_cancelled", "ipc_timeout"]
        screenLocked = checked && !unknownLockCodes.contains(code) ? capabilities["screen_locked"]?.bool : nil
    }
    var description: String {
        guard checked else { return "未取得带真实检查时间的能力证据；权限与桌面状态未知。" }
        let time = checkedAt!.formatted(date: .abbreviated, time: .standard)
        let screen = screenLocked.map { $0 ? "已核验锁定" : "已核验未锁定" } ?? "未知"
        return "实际检查：\(time)；辅助功能：\(permission.title)；主应用 IPC：\(bridgeAvailable ? "可用" : "未确认可用")；屏幕：\(screen)；结果代码：\(code.isEmpty ? "未知" : code)。只读能力不是目标验证或发送授权。"
    }
}
