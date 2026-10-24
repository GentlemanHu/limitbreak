import SwiftUI

var consentExplanation: String {
    L("额度恢复后，对「智能托管」的会话：定位到它原来的窗口 → 确认输入框为空且你没有在打字 → 写入该会话的继续文案并发送 → 等待会话回执。\n\n任何一步不确定都会停下并提示你，不会重复发送。随时可一键紧急暂停，或把单个会话改为「仅监控」。",
      "When quota recovers, for 'Hosted' sessions: locate original window → confirm input box is empty and you are not typing → type the continue message and send → wait for session turn receipt.\n\nHalts and notifies you if anything is uncertain; never resends automatically. Can be emergency paused at any time or set to 'Monitor only'.")
}

// Default hosting for sessions that follow the global rule. Turning it on
// before consent asks for consent in place instead of sending the user away.
struct DefaultHostingToggle: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    var showsLabel = true
    @State private var busy = false
    @State private var askingConsent = false
    var body: some View {
        HStack(spacing: 8) {
            if busy { ProgressView().controlSize(.small) }
            if showsLabel {
                let onStr = L("默认托管 · 开", "Default Hosting · On")
                let offStr = L("默认托管 · 关", "Default Hosting · Off")
                Text(runtime.defaultHosted ? onStr : offStr).font(.callout).foregroundStyle(runtime.defaultHosted ? .primary : .secondary)
            }
            Toggle(L("默认托管", "Default Hosting"), isOn: Binding(get: { runtime.defaultHosted }, set: { value in
                if value && !runtime.automationConsented { askingConsent = true; return }
                busy = true
                Task { await runtime.setDefaultHosting(value); busy = false }
            })).toggleStyle(.switch).labelsHidden()
        }
        .disabled(busy || !runtime.localServiceFresh)
        .help(runtime.defaultHosted ? L("关闭：跟随全局的会话改为仅监控；单独设为「智能托管」的会话不受影响", "Disable: sessions following global become monitor-only; individually hosted sessions unaffected") : L("开启：跟随全局的会话在额度恢复后自动继续；单独设置的会话不受影响", "Enable: sessions following global auto-continue on quota recovery; individually set sessions unaffected"))
        .alert(L("允许 LimitBreak 自动发送继续文案？", "Allow LimitBreak to automatically send continue messages?"), isPresented: $askingConsent) {
            Button(L("取消", "Cancel"), role: .cancel) {}
            Button(L("允许并开启默认托管", "Allow and Enable Default Hosting")) {
                busy = true
                Task {
                    if await runtime.grantAutomationConsent() { await runtime.setDefaultHosting(true) }
                    busy = false
                }
            }
        } message: { Text(consentExplanation) }
    }
}

// Stopping is one click; resuming is one click. Neither touches per-session settings.
struct EmergencyPauseButton: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    var compact = false
    @State private var busy = false
    var body: some View {
        let resumeLabel = compact ? L("恢复", "Resume") : L("恢复自动发送", "Resume Auto-Send")
        let pauseLabel = compact ? L("暂停", "Pause") : L("紧急暂停", "Emergency Pause")
        Button {
            busy = true
            Task { await runtime.setEmergencyPause(!runtime.paused); busy = false }
        } label: {
            Label(runtime.paused ? resumeLabel : pauseLabel,
                  systemImage: runtime.paused ? "play.circle.fill" : "stop.circle")
        }
        .tint(runtime.paused ? .green : .red)
        .disabled(busy || !runtime.localServiceFresh)
        .help(runtime.paused ? L("恢复后，各会话按自己的托管设置自动继续", "Once resumed, sessions auto-continue according to their own hosting settings") : L("立即停止所有自动发送；额度监控和排队记录保留", "Immediately halts all automatic sends; quota monitoring and queue records remain"))
    }
}

// Shown on every workbench page while nothing can be sent automatically.
struct HostingBanner: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    var body: some View {
        if runtime.localServiceFresh && runtime.paused {
            bar(icon: "pause.octagon.fill", color: .red, text: L("已紧急暂停：所有自动发送已停止，额度监控和排队照常进行。", "Emergency paused: all automatic sends stopped; quota monitoring and queueing continue normally.")) { EmergencyPauseButton(runtime: runtime).buttonStyle(.borderedProminent) }
        } else if runtime.localServiceFresh && !runtime.automationConsented && runtime.hostedSessionCount > 0 {
            bar(icon: "hand.raised.fill", color: .orange, text: L("\(runtime.hostedSessionCount) 个会话设为智能托管，但尚未允许自动继续，目前只监控。", "\(runtime.hostedSessionCount) session(s) set to Hosted, but auto-continue is not yet authorized; currently monitor only.")) { ConsentButton(runtime: runtime) }
        }
    }
    private func bar<Action: View>(icon: String, color: Color, text: String, @ViewBuilder action: () -> Action) -> some View {
        HStack(spacing: 10) {
            Image(systemName: icon).foregroundStyle(color)
            Text(text).font(.callout)
            Spacer()
            action().controlSize(.small)
        }.padding(.horizontal, 14).padding(.vertical, 9).background(color.opacity(0.08))
    }
}
