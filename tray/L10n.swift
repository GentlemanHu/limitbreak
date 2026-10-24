import Foundation
import SwiftUI

// Supported languages in LimitBreak.
enum AppLanguage: String, CaseIterable, Identifiable {
    case system = "system"
    case zh = "zh-Hans"
    case en = "en"

    var id: String { rawValue }

    var title: String {
        switch self {
        case .system: return LanguageManager.shared.isChinese ? "跟随系统" : "System Default"
        case .zh: return "简体中文"
        case .en: return "English"
        }
    }
}

// Reactive language manager backed by UserDefaults (@AppStorage).
final class LanguageManager: ObservableObject {
    static let shared = LanguageManager()

    @AppStorage("app_language") var preference: String = "system" {
        didSet {
            objectWillChange.send()
        }
    }

    var isChinese: Bool {
        if preference == "zh-Hans" { return true }
        if preference == "en" { return false }
        let preferred = Locale.preferredLanguages.first ?? "en"
        return preferred.hasPrefix("zh")
    }

    var locale: Locale {
        isChinese ? Locale(identifier: "zh_CN") : Locale(identifier: "en_US")
    }

    var defaultContinuePrompt: String {
        isChinese ? "继续" : "Continue"
    }

    func s(_ zh: String, _ en: String) -> String {
        isChinese ? zh : en
    }

    // Bilingual translation for canonical daemon reason codes.
    func explain(reason: String, reasonText: String? = nil) -> String {
        if isChinese {
            if let text = reasonText, !text.isEmpty { return text }
            return explainZh(reason)
        } else {
            return explainEn(reason)
        }
    }

    private func explainZh(_ reason: String) -> String {
        if reason.hasPrefix("waiting for desktop: ") {
            let rest = String(reason.dropFirst("waiting for desktop: ".count))
            let code = rest.split(separator: " ").first.map(String.init) ?? rest
            return "暂未发送：" + (desktopWaitsZh[code] ?? rest)
        }
        if reason.hasPrefix("account concurrency limit: ") {
            let rest = String(reason.dropFirst("account concurrency limit: ".count))
            if let holders = rest.components(separatedBy: "held by ").last, !holders.isEmpty {
                return "账号并发已满：\(holders) 正在用同一账号运行，结束后自动继续"
            }
            return "账号并发已满，排队中"
        }
        for (prefix, zh) in reasonMapZh where reason.hasPrefix(prefix) {
            return zh
        }
        return reason
    }

    private func explainEn(_ reason: String) -> String {
        if reason.hasPrefix("waiting for desktop: ") {
            let rest = String(reason.dropFirst("waiting for desktop: ".count))
            let code = rest.split(separator: " ").first.map(String.init) ?? rest
            return "Waiting: " + (desktopWaitsEn[code] ?? rest)
        }
        if reason.hasPrefix("account concurrency limit: ") {
            let rest = String(reason.dropFirst("account concurrency limit: ".count))
            if let holders = rest.components(separatedBy: "held by ").last, !holders.isEmpty {
                return "Account concurrency limit reached: held by \(holders); queued until completion"
            }
            return "Account concurrency limit reached; queued"
        }
        for (prefix, en) in reasonMapEn where reason.hasPrefix(prefix) {
            return en
        }
        return reason
    }

    private let desktopWaitsZh: [String: String] = [
        "user_active": "你正在使用键盘或鼠标；停手 3 秒后自动重试",
        "claude_in_use": "你正在使用 Claude；Claude 空闲一段时间后再发送",
        "screen_locked": "屏幕已锁定；解锁后自动重试",
        "screen_inactive": "屏幕未处于活动状态；唤醒后自动重试",
        "console_inactive": "当前登录会话不在前台；切回后自动重试",
        "accessibility_denied": "缺少辅助功能权限；在概览中授权后自动重试",
        "bridge_unavailable": "主程序发送通道未就绪；确认 LimitBreak.app 正在运行",
        "session_busy": "会话正在处理上一轮；空闲后自动在后台直发",
        "terminal_busy": "终端会话正在运行；结束本轮后自动重试"
    ]

    private let desktopWaitsEn: [String: String] = [
        "user_active": "You are using the keyboard or mouse; retrying after 3s idle",
        "claude_in_use": "Claude is in use; waiting until Claude is idle before sending",
        "screen_locked": "Screen is locked; retrying after unlock",
        "screen_inactive": "Screen is inactive; retrying after display wakes",
        "console_inactive": "Login session is not in foreground; retrying after switch back",
        "accessibility_denied": "Accessibility permission missing; authorize in Overview",
        "bridge_unavailable": "Main app bridge unavailable; ensure LimitBreak.app is running",
        "session_busy": "Session is processing previous turn; will send once idle",
        "terminal_busy": "Terminal session is busy; will retry after current turn ends"
    ]

    private let reasonMapZh: [(String, String)] = [
        ("atomic desktop and resource lease acquired", "已锁定目标与账号，正在发送"),
        ("binding changed", "绑定已修改"),
        ("confirmed by", "已由会话 Hook 确认发送成功"),
        ("dispatch confirmation timed out", "发送后未收到会话确认；不会重发，请人工查看"),
        ("hook model differs", "会话模型与绑定不一致"),
        ("model changed", "会话模型已切换"),
        ("native quota auto-resume", "原生自动继续处理中"),
        ("non-quota StopFailure", "非额度原因中断，需人工查看"),
        ("normal stop", "已正常结束本轮"),
        ("obsolete generation", "已过期（会话有新动态）"),
        ("prompt submitted", "运行中"),
        ("rate limit", "额度耗尽，排队等待恢复"),
        ("session started", "会话已开始"),
        ("session ended", "会话已结束"),
        ("submission failed or uncertain", "发送失败或结果不确定；不会重发，请人工查看"),
        ("submitted; waiting for a fresh hook", "已发送，等待会话确认"),
        ("submitted via direct channel", "已后台静默直发，等待会话回执"),
        ("submitted via the Claude window", "已通过 Claude 窗口发送，等待会话回执"),
        ("waiting for quota reset", "等待额度重置"),
        ("unknown or stale quota evidence", "额度数据过期，等待刷新"),
        ("account concurrency limit", "账号并发已满，排队中"),
        ("quota ready", "额度可用"),
        ("no explicit binding", "尚未绑定来源"),
        ("only code sessions can auto-resume", "仅 Code 会话可自动继续"),
        ("new prompt superseded", "用户已手动发送新指令"),
        ("cancelled by user", "已由用户取消"),
        ("awaiting a session hook", "等待会话上报")
    ]

    private let reasonMapEn: [(String, String)] = [
        ("atomic desktop and resource lease acquired", "Target and account locked; sending now"),
        ("binding changed", "Binding has changed"),
        ("confirmed by", "Send confirmed by session hook"),
        ("dispatch confirmation timed out", "Confirmation timed out; will not resend to avoid duplicates"),
        ("hook model differs", "Session model differs from binding"),
        ("model changed", "Session model has changed"),
        ("native quota auto-resume", "Native auto-resume handling"),
        ("non-quota StopFailure", "Interrupted for non-quota reason; check manually"),
        ("normal stop", "Round finished normally"),
        ("obsolete generation", "Superseded by newer session activity"),
        ("prompt submitted", "Running"),
        ("rate limit", "Rate limited; queued waiting for fresh quota"),
        ("session started", "Session started"),
        ("session ended", "Session ended"),
        ("submission failed or uncertain", "Submission failed or uncertain; will not resend"),
        ("submitted; waiting for a fresh hook", "Submitted; waiting for session hook confirmation"),
        ("submitted via direct channel", "Submitted via direct socket; waiting for turn receipt"),
        ("submitted via the Claude window", "Submitted via Claude window; waiting for turn receipt"),
        ("waiting for quota reset", "Waiting for quota reset check"),
        ("unknown or stale quota evidence", "Quota data is stale; waiting for refresh"),
        ("account concurrency limit", "Account concurrency limit reached; queued"),
        ("quota ready", "Quota available"),
        ("no explicit binding", "No quota source bound"),
        ("only code sessions can auto-resume", "Only Code sessions support auto-resume"),
        ("new prompt superseded", "Superseded by user's manual message"),
        ("cancelled by user", "Cancelled by user"),
        ("awaiting a session hook", "Awaiting session hook event")
    ]
}

// Global convenience accessor for concise inline bilingual strings.
func L(_ zh: String, _ en: String) -> String {
    LanguageManager.shared.s(zh, en)
}

func localizedState(_ state: String) -> String {
    if LanguageManager.shared.isChinese {
        return trayState(state)
    }
    let en = ["RUNNING": "Running", "COOLDOWN": "Waiting for quota", "READY_TO_RESUME": "Ready to resume",
     "VERIFYING": "Verifying delivery", "DISPATCHING": "Restoring", "COMPLETED": "Completed",
     "CANCELLED": "Stopped", "NEEDS_ATTENTION": "Needs attention", "allowed": "Quota available",
     "limited": "Rate limited", "unknown": "Quota unknown", "disabled": "Disabled", "stale": "Checking", "missing": "Unavailable"]
    return en[state] ?? state
}

func localizedDuration(_ seconds: TimeInterval) -> String {
    if LanguageManager.shared.isChinese { return trayDuration(seconds) }
    let s = max(0, Int(seconds))
    if s >= 86400 { return "\(s / 86400)d " + String(format: "%02d:%02d", s % 86400 / 3600, s % 3600 / 60) }
    return String(format: "%02d:%02d:%02d", s / 3600, s % 3600 / 60, s % 60)
}

extension TrayHostMode {
    var localizedTitle: String {
        if LanguageManager.shared.isChinese { return title }
        switch self {
        case .follow: return "Follow Global"
        case .hosted: return "Hosted (Auto-continue)"
        case .monitor: return "Monitor Only"
        case .off: return "Off"
        }
    }
    static func localizedTitle(_ raw: String) -> String {
        if LanguageManager.shared.isChinese { return title(raw) }
        return raw == "legacy" ? "Legacy Binding" : (TrayHostMode(rawValue: raw)?.localizedTitle ?? raw)
    }
}
