import SwiftUI

// Read-only production checks. Real sends are only exercised per session via 「测试发送」.
struct WorkbenchDiagnostics: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var probeBusy = false
    @State private var probeResult = L("尚未执行本次只读检查", "Read-only probe has not been run yet")
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 24) {
                Text(L("诊断", "Diagnostics")).font(.title.bold())
                Text(L("只读检查各环节是否就绪。要验证某个会话能否真正收到继续文案，请在「会话托管」里对它使用「测试发送」。",
                       "Read-only health checks. To verify that a session receives continue prompts, use 'Test Send' in Sessions.")).foregroundStyle(.secondary)
                GroupBox(L("运行状态", "System Status")) {
                    VStack(alignment: .leading, spacing: 12) {
                        statusRow(L("自有后台", "Owned Daemon"), value: runtime.daemonRunning ? L("运行中", "Running") : L("未运行", "Stopped"), good: runtime.daemonRunning)
                        statusRow(L("本地快照", "Local Snapshot"), value: runtime.localServiceFresh ? L("近期收到", "Fresh") : L("未收到／过期", "Stale / Missing"), good: runtime.localServiceFresh)
                        statusRow(L("额度来源", "Quota Source"), value: runtime.proxyConnected ? L("至少有来源连接", "Connected") : L("尚未确认", "Unconfirmed"), good: runtime.proxyConnected)
                        statusRow(L("辅助功能", "Accessibility"), value: runtime.accessibilityGranted ? L("已授权", "Authorized") : L("未授权", "Unauthorized"), good: runtime.accessibilityGranted)
                        statusRow(L("会话事件", "Session Events"), value: runtime.hookStatus, good: runtime.hooksInstalled)
                        statusRow(L("自动继续授权", "Auto-Continue Authorization"), value: runtime.automationConsented ? L("已允许", "Authorized") : L("未允许（只监控）", "Unauthorized (Monitor only)"), good: runtime.automationConsented)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox(L("Claude 桌面", "Claude Desktop")) {
                    VStack(alignment: .leading, spacing: 12) {
                        HStack {
                            Button(probeBusy ? L("检查中…", "Probing…") : L("只读检查桌面", "Probe Desktop (Read-only)")) { Task { await probe() } }.disabled(probeBusy || !runtime.daemonRunning)
                            Text(L("不切换会话，不写输入框，不发送消息", "Does not switch sessions, type, or send messages")).font(.caption).foregroundStyle(.secondary)
                        }
                        Text(probeResult).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                        if runtime.snapshot?.capabilities["bridge_checked"]?.bool == true {
                            Text(GuardDesktopObservation(runtime.snapshot?.capabilities ?? [:], now: runtime.now).description).font(.caption)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
            }.padding(28).frame(maxWidth: 920, alignment: .leading)
        }
    }
    private func statusRow(_ title: String, value: String, good: Bool) -> some View {
        HStack { Text(title); Spacer(); Label(value, systemImage: good ? "checkmark.circle" : "minus.circle").foregroundStyle(good ? Color.green : Color.secondary) }
    }
    private func probe() async {
        guard !probeBusy else { return }; probeBusy = true; defer { probeBusy = false }
        do {
            let response = try JSONDecoder().decode(GuardDesktopProbeResponse.self, from: await runtime.request("/api/desktop/probe", body: Data("{}".utf8)))
            guard response.completed else { throw RuntimeFailure(L("本次桌面检查未确认完成，状态未知", "Desktop probe incomplete; status unknown")) }
            runtime.checkAccessibility()
            await runtime.refresh()
            probeResult = GuardDesktopObservation(response.result, now: Date()).description
        }
        catch { probeResult = (error as? RuntimeFailure)?.message ?? L("桌面检查未完成；未发送任何消息。", "Desktop probe incomplete; no messages sent.") }
    }
}
