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
        case .direct: return "后台直发"
        case .wokenDirect: return "唤醒后直发"
        case .windowAfterWake: return "界面发送（进程未启动）"
        case .window: return terminal ? "终端发送" : "界面发送"
        }
    }
    func detail(terminal: Bool = false) -> String {
        switch self {
        case .direct: return "会话进程在运行，经会话自带的消息端口静默送达；未切换窗口、未动键盘"
        case .wokenDirect: return "会话进程原本未运行：先在后台打开会话让 Claude 启动它，再经消息端口静默送达；未输入任何内容"
        case .windowAfterWake: return "后台唤醒后会话进程始终没有启动，改为打开会话、核验输入框后输入发送"
        case .window: return terminal ? "输入到会话所在的 tmux 窗格，或在后台用 claude -p --resume 续写" : "打开会话、核验输入框为空后输入发送（后台直发已关闭或不可用）"
        }
    }
    // One line for compact cells.
    func summary(terminal: Bool = false) -> String {
        switch self {
        case .direct: return "进程在运行，静默送达"
        case .wokenDirect: return "后台唤醒会话后静默送达"
        case .windowAfterWake: return "进程未启动，改为界面输入"
        case .window: return terminal ? "tmux 输入或后台续写" : "打开会话后输入"
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

    func deliveryPlan(for session: TraySession) -> DeliveryPlan {
        if session.origin?.app == "terminal" {
            return DeliveryPlan(title: "终端发送", summary: "tmux 输入或后台续写", detail: "输入到 tmux 窗格，或会话已关闭时在后台续写", icon: "terminal", color: .blue)
        }
        guard directDeliveryEnabled else {
            return DeliveryPlan(title: "界面发送", summary: "后台直发已关闭", detail: "后台静默直发已在「托管规则 › 发送方式」关闭", icon: "keyboard", color: .blue)
        }
        var detail = "会话进程在运行时静默直发；未运行时先在后台唤醒会话再直发"
        if snapshot?.policy["restore_focus"]?.string != "off" { detail += "，并把焦点还给原应用" }
        return DeliveryPlan(title: "后台直发", summary: "未运行时先后台唤醒", detail: detail, icon: "bolt.horizontal.circle", color: .green)
    }
}

// A compact capsule naming the channel, recorded or planned.
struct DeliveryBadge: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession?
    let job: TrayJob?
    var body: some View {
        let terminal = session?.origin?.app == "terminal"
        if let channel = DeliveryChannel(job: job) {
            badge(channel.title(terminal: terminal), icon: channel.icon, color: channel.color).help("本次发送方式：" + channel.detail(terminal: terminal))
        } else if let session, job?.active ?? true {
            let plan = runtime.deliveryPlan(for: session)
            badge("将" + plan.title, icon: plan.icon, color: plan.color.opacity(0.8)).help("计划的发送方式：" + plan.detail)
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
    let session: TraySession
    var body: some View {
        let terminal = session.origin?.app == "terminal"
        let hosted = ["hosted", "legacy"].contains(session.host?.mode ?? "")
        if let job = runtime.lastDelivery(for: session), let channel = DeliveryChannel(job: job) {
            row(job.kind == "test" ? "上次测试发送" : "上次发送方式", channel.title(terminal: terminal), channel.detail(terminal: terminal),
                icon: channel.icon, color: channel.color, at: trayDate(job.updated_at))
        }
        if hosted {
            let plan = runtime.deliveryPlan(for: session)
            row("下次发送方式", plan.title, plan.detail, icon: plan.icon, color: plan.color, at: nil)
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
