import AppKit
import ServiceManagement
import SwiftUI

struct WorkbenchSettings: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var atLogin = SMAppService.mainApp.status == .enabled
    @State private var connection = false
    @State private var restarting = false
    @AppStorage(DockPresence.key) private var alwaysInDock = false
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                Text("设置").font(.title.bold())
                GroupBox("连接与后台") {
                    VStack(alignment: .leading, spacing: 12) {
                        LabeledContent("本地后台", value: runtime.daemonRunning ? "运行中" : "未运行")
                        Text(runtime.daemonRecoveryStatus).font(.caption).foregroundStyle(.secondary)
                        LabeledContent("CLIProxyAPI", value: runtime.proxyURL)
                        HStack {
                            Button("修改 CLIProxyAPI 连接") { connection = true }
                            Button(restarting ? "重启中…" : "重启后台") {
                                restarting = true
                                Task { await runtime.restartOwnedDaemon(); restarting = false }
                            }.disabled(restarting || runtime.connecting)
                        }
                        Text("重启后自动恢复你上次的托管开关。后台异常退出最多自动恢复 3 次；关闭窗口不停止监控，退出应用只停止自己的后台。").font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox("权限与会话事件") {
                    VStack(alignment: .leading, spacing: 14) {
                        LabeledContent("辅助功能", value: runtime.accessibilityObservation.state.title)
                        AccessibilityActions(runtime: runtime)
                        Divider()
                        LabeledContent("会话事件", value: runtime.hookStatus)
                        HookInstallButton(runtime: runtime)
                        Text("通过辅助功能把逐会话文案写入原窗口的输入框并按发送按钮，不使用鼠标坐标或剪贴板。输入框非空、你正在打字或目标不唯一时会停下。").font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                WorkbenchPolicySettings(runtime: runtime)
                GroupBox("系统行为") {
                    VStack(alignment: .leading, spacing: 14) {
                        Toggle("保持系统唤醒，允许显示器关闭", isOn: Binding(get: { runtime.awake }, set: { _ in runtime.toggleAwake() }))
                        Toggle("登录时启动", isOn: Binding(get: { atLogin }, set: changeLogin))
                        Toggle("始终在程序坞显示图标", isOn: $alwaysInDock).onChange(of: alwaysInDock) { DockPresence.apply(windowOpen: true) }
                        Text("默认只在工作台打开时显示程序坞图标，关闭工作台后只留在菜单栏。").font(.caption).foregroundStyle(.secondary)
                        Text("不会自动解锁屏幕，不阻止合盖或手动睡眠。登录项需要的系统批准由你确认。").font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox("关于") {
                    VStack(alignment: .leading, spacing: 12) {
                        Button("MCP 授权与逐操作审批") { runtime.openWorkbench(.mcp) }
                        Text("版本 " + (Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "开发构建")).font(.caption.monospaced())
                        Button("退出 LimitBreak") { NSApplication.shared.terminate(nil) }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
            }.padding(28).frame(maxWidth: 900, alignment: .leading)
        }.sheet(isPresented: $connection) { ConnectionSheet(runtime: runtime, close: { connection = false }) }
    }
    private func changeLogin(_ enabled: Bool) {
        do {
            if enabled { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
            atLogin = SMAppService.mainApp.status == .enabled
            if enabled && SMAppService.mainApp.status == .requiresApproval { SMAppService.openSystemSettingsLoginItems(); runtime.error = "请在系统登录项中确认；批准前不会显示为已开启。" }
        } catch { runtime.error = "未能修改登录项，请先将应用放入应用程序目录再确认。" }
    }
}
