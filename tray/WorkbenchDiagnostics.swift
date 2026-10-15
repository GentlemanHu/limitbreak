import SwiftUI

// Read-only production checks. Real sends are only exercised per session via 「测试发送」.
struct WorkbenchDiagnostics: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var probeBusy = false
    @State private var probeResult = "尚未执行本次只读检查"
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 24) {
                Text("诊断").font(.title.bold())
                Text("只读检查各环节是否就绪。要验证某个会话能否真正收到继续文案，请在「会话托管」里对它使用「测试发送」。").foregroundStyle(.secondary)
                GroupBox("运行状态") {
                    VStack(alignment: .leading, spacing: 12) {
                        statusRow("自有后台", value: runtime.daemonRunning ? "运行中" : "未运行", good: runtime.daemonRunning)
                        statusRow("本地快照", value: runtime.localServiceFresh ? "近期收到" : "未收到／过期", good: runtime.localServiceFresh)
                        statusRow("额度来源", value: runtime.proxyConnected ? "至少有来源连接" : "尚未确认", good: runtime.proxyConnected)
                        statusRow("辅助功能", value: runtime.accessibilityGranted ? "已授权" : "未授权", good: runtime.accessibilityGranted)
                        statusRow("会话事件", value: runtime.hookStatus, good: runtime.hooksInstalled)
                        statusRow("自动继续授权", value: runtime.automationConsented ? "已允许" : "未允许（只监控）", good: runtime.automationConsented)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox("Claude 桌面") {
                    VStack(alignment: .leading, spacing: 12) {
                        HStack {
                            Button(probeBusy ? "检查中…" : "只读检查桌面") { Task { await probe() } }.disabled(probeBusy || !runtime.daemonRunning)
                            Text("不切换会话，不写输入框，不发送消息").font(.caption).foregroundStyle(.secondary)
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
            guard response.completed else { throw RuntimeFailure("本次桌面检查未确认完成，状态未知") }
            runtime.checkAccessibility()
            await runtime.refresh()
            probeResult = GuardDesktopObservation(response.result, now: Date()).description
        }
        catch { probeResult = (error as? RuntimeFailure)?.message ?? "桌面检查未完成；未发送任何消息。" }
    }
}
