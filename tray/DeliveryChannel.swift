import SwiftUI

// How a continue reached its session. The daemon records it on the job once a
// send was handed to an executor (job.delivery); before that the panels show
// the channel the current policy will try first.
enum DeliveryChannel: String {
    case direct
    case wokenDirect = "woken_direct"
    case windowAfterWake = "window_after_wake"
    case window

    init?(job: TrayJob?) {
        guard let raw = job?.delivery, let channel = DeliveryChannel(rawValue: raw) else { return nil }
        self = channel
    }

    func title(terminal: Bool = false) -> String {
        switch self {
        case .direct: return L("后台直发", "Direct Socket")
        case .wokenDirect: return L("唤醒后直发", "Direct after Wake")
        case .windowAfterWake: return L("界面发送（进程未启动）", "Window Delivery (Process not started)")
        case .window: return terminal ? L("终端发送", "Terminal Delivery") : L("界面发送", "Window Delivery")
        }
    }
    func detail(terminal: Bool = false) -> String {
        switch self {
        case .direct: return L("会话进程在运行，经会话自带的消息端口静默送达；未切换窗口、未动键盘", "Process is running; delivered silently through Claude's cross-session socket without window switching or keyboard input")
        case .wokenDirect: return L("会话进程原本未运行：先在后台打开会话让 Claude 启动它，再经消息端口静默送达；未输入任何内容", "Process was not running: woken in background first for Claude to launch, then delivered silently via socket without typing")
        case .windowAfterWake: return L("后台唤醒后会话进程始终没有启动，改为打开会话、核验输入框后输入发送", "Process failed to start after wake; falling back to opening session window, verifying empty input box, and typing")
        case .window: return terminal
            ? L("输入到会话所在的 tmux 窗格，或在后台用 claude -p --resume 续写", "Types into session's tmux pane or resumes in background with claude -p --resume")
            : L("打开会话、核验输入框为空后输入发送（后台直发已关闭、不可用，或会话的权限模式会扣下直发消息）", "Opens session, verifies input box is empty, and types (direct delivery disabled, unavailable, or held by permission mode)")
        }
    }
    // One line for compact cells.
    func summary(terminal: Bool = false) -> String {
        switch self {
        case .direct: return L("进程在运行，静默送达", "Process running, silent delivery")
        case .wokenDirect: return L("后台唤醒会话后静默送达", "Woken in background, silent delivery")
        case .windowAfterWake: return L("进程未启动，改为界面输入", "Process unstarted, window typing")
        case .window: return terminal ? L("tmux 输入或后台续写", "tmux typing or background resume") : L("打开会话后输入", "Open session and type")
        }
    }
    var icon: String {
        switch self {
        case .direct: return "bolt.horizontal.circle.fill"
        case .wokenDirect: return "sunrise.circle.fill"
        case .windowAfterWake: return "macwindow.badge.plus"
        case .window: return "keyboard"
        }
    }
    var color: Color {
        switch self {
        case .direct: return .green
        case .wokenDirect: return .teal
        case .windowAfterWake: return .orange
        case .window: return .blue
        }
    }
}

// What the panels say before anything was sent.
struct DeliveryPlan {
    let title: String
    let summary: String
    let detail: String
    let icon: String
    let color: Color
}

extension TrayRuntime {
    // The latest recorded delivery for the session, resume or test send.
    func lastDelivery(for session: TraySession) -> TrayJob? { index.lastDelivery[session.id] }

    var directDeliveryEnabled: Bool { snapshot?.policy["direct_delivery"]?.string != "off" }

    // Mirrors domain.DirectAccepted: only sessions reporting a prompting
    // permission mode take a direct message without holding it for approval.
    func directAccepted(_ session: TraySession) -> Bool {
        ["default", "acceptEdits", "auto", "dontAsk"].contains(session.permission_mode ?? "")
    }

    // Why a hosted Claude session will be typed into instead of sent directly.
    func directHeldReason(_ session: TraySession) -> String {
        if session.permission_mode == "bypassPermissions" {
            return L("会话处于「跳过权限确认」模式：Claude 会把不带发送方权限模式的直发消息扣下待批准，桌面会话无法批准、到期作废，所以改为打开会话后输入", "Session runs in 'bypass permissions' mode: Claude holds direct messages without sender mode for approval which cannot be approved on desktop, so window delivery is used instead")
        }
        if let mode = session.permission_mode, !mode.isEmpty {
            return L("会话处于 \(mode) 模式，直发消息可能被扣下待批准，所以改为打开会话后输入", "Session runs in \(mode) mode where direct messages may be held for approval, so window delivery is used instead")
        }
        return L("会话还未上报权限模式（下一次该会话的 Hook 后可知），为免直发被扣下，先用打开会话后输入", "Session permission mode unknown yet (reported after next Hook); using window delivery to prevent message holds")
    }

    func deliveryPlan(for session: TraySession) -> DeliveryPlan {
        if session.origin?.app == "terminal" {
            return DeliveryPlan(title: L("终端发送", "Terminal Delivery"), summary: L("tmux 输入或后台续写", "tmux typing or background resume"), detail: L("输入到 tmux 窗格，或会话已关闭时在后台续写", "Types into tmux pane or resumes in background when session is closed"), icon: "terminal", color: .blue)
        }
        guard directDeliveryEnabled else {
            return DeliveryPlan(title: L("界面发送", "Window Delivery"), summary: L("后台直发已关闭", "Direct delivery disabled"), detail: L("后台静默直发已在「托管规则 › 发送方式」关闭", "Silent direct delivery disabled in Hosting Rules"), icon: "keyboard", color: .blue)
        }
        guard directAccepted(session) else {
            let summary = session.permission_mode == "bypassPermissions" ? L("跳过权限模式不收直发", "Bypass mode holds direct") : L("权限模式未知，先用界面", "Unknown mode, using window")
            return DeliveryPlan(title: L("界面发送", "Window Delivery"), summary: summary, detail: directHeldReason(session), icon: "keyboard", color: .blue)
        }
        var detail = L("会话进程在运行时静默直发；未运行时先在后台唤醒会话再直发", "Delivers directly via socket when running; wakes session in background first when idle")
        if snapshot?.policy["restore_focus"]?.string != "off" { detail += L("，并把焦点还给原应用", ", and restores focus to original app") }
        return DeliveryPlan(title: L("后台直发", "Direct Socket"), summary: L("未运行时先后台唤醒", "Wakes in background if idle"), detail: detail, icon: "bolt.horizontal.circle", color: .green)
    }
}

// A compact capsule naming the channel, recorded or planned.
struct DeliveryBadge: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    let session: TraySession?
    let job: TrayJob?
    var body: some View {
        let terminal = session?.origin?.app == "terminal"
        if let channel = DeliveryChannel(job: job) {
            badge(channel.title(terminal: terminal), icon: channel.icon, color: channel.color).help(L("本次发送方式：", "Delivery method: ") + channel.detail(terminal: terminal))
        } else if let session, job?.active ?? true {
            let plan = runtime.deliveryPlan(for: session)
            let prefix = lang.isChinese ? "将" : "Will "
            badge(prefix + plan.title, icon: plan.icon, color: plan.color.opacity(0.8)).help(L("计划的发送方式：", "Planned delivery: ") + plan.detail)
        }
    }
    private func badge(_ text: String, icon: String, color: Color) -> some View {
        Label(text, systemImage: icon).font(.caption).foregroundStyle(color).lineLimit(1)
            .padding(.horizontal, 7).padding(.vertical, 2).background(color.opacity(0.12), in: Capsule())
    }
}

// The session panel's "how does it get sent" lines: the last recorded delivery
// and, for a hosted session, what the current policy will try next time.
struct SessionDeliveryInfo: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    let session: TraySession
    var body: some View {
        let terminal = session.origin?.app == "terminal"
        let hosted = ["hosted", "legacy"].contains(session.host?.mode ?? "")
        if let job = runtime.lastDelivery(for: session), let channel = DeliveryChannel(job: job) {
            let label = job.kind == "test" ? L("上次测试发送", "Last Test Send") : L("上次发送方式", "Last Delivery")
            row(label, channel.title(terminal: terminal), channel.detail(terminal: terminal),
                icon: channel.icon, color: channel.color, at: trayDate(job.updated_at))
        }
        if hosted {
            let plan = runtime.deliveryPlan(for: session)
            row(L("下次发送方式", "Next Delivery"), plan.title, plan.detail, icon: plan.icon, color: plan.color, at: nil)
        }
    }
    private func row(_ label: String, _ title: String, _ detail: String, icon: String, color: Color, at: Date?) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(label).font(.caption).foregroundStyle(.secondary).frame(width: 86, alignment: .leading)
            Label(title, systemImage: icon).font(.callout.weight(.medium)).foregroundStyle(color)
            Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
            if let at { Text(trayRelative(at, now: runtime.now)).font(.caption2).foregroundStyle(.tertiary) }
        }
    }
}
