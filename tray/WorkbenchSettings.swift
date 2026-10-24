import AppKit
import ServiceManagement
import SwiftUI

struct WorkbenchSettings: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    @State private var atLogin = SMAppService.mainApp.status == .enabled
    @State private var connection = false
    @State private var restarting = false
    @AppStorage(DockPresence.key) private var alwaysInDock = false
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                Text(L("设置", "Settings")).font(.title.bold())
                GroupBox(L("语言与界面", "Language & Appearance")) {
                    VStack(alignment: .leading, spacing: 12) {
                        HStack {
                            Text(L("界面语言", "Interface Language"))
                            Spacer()
                            Picker("", selection: $lang.preference) {
                                ForEach(AppLanguage.allCases) { item in
                                    Text(item.title).tag(item.rawValue)
                                }
                            }.pickerStyle(.menu).frame(width: 140)
                        }
                        Text(L("默认跟随 macOS 系统语言。切换后即时生效，无需重启应用。", "Follows macOS system language by default. Takes effect immediately without restarting.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox(L("连接与后台", "Connection & Background Daemon")) {
                    VStack(alignment: .leading, spacing: 12) {
                        LabeledContent(L("本地后台", "Local Daemon"), value: runtime.daemonRunning ? L("运行中", "Running") : L("未运行", "Not running"))
                        Text(runtime.daemonRecoveryStatus).font(.caption).foregroundStyle(.secondary)
                        LabeledContent("CLIProxyAPI", value: runtime.proxyURL)
                        HStack {
                            Button(L("修改 CLIProxyAPI 连接", "Edit CLIProxyAPI Connection")) { connection = true }
                            Button(restarting ? L("重启中…", "Restarting…") : L("重启后台", "Restart Daemon")) {
                                restarting = true
                                Task { await runtime.restartOwnedDaemon(); restarting = false }
                            }.disabled(restarting || runtime.connecting)
                        }
                        Text(L("重启后自动恢复你上次的托管开关。后台异常退出最多自动恢复 3 次；关闭窗口不停止监控，退出应用只停止自己的后台。", "Automatically restores your last hosting toggle after restart. Recovers up to 3 times on daemon crash. Closing the window does not stop monitoring; quitting the app only stops its owned daemon.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox(L("权限与会话事件", "Permissions & Session Events")) {
                    VStack(alignment: .leading, spacing: 14) {
                        LabeledContent(L("辅助功能", "Accessibility"), value: runtime.accessibilityObservation.state.title)
                        AccessibilityActions(runtime: runtime)
                        Divider()
                        LabeledContent(L("会话事件", "Session Events"), value: runtime.hookStatus)
                        HookInstallButton(runtime: runtime)
                        Text(L("通过辅助功能把逐会话文案写入原窗口的输入框并按发送按钮，不使用鼠标坐标或剪贴板。输入框非空、你正在打字或目标不唯一时会停下。", "Writes per-session continue prompts into the target window using Accessibility and presses send. Does not rely on mouse coordinates or clipboard. Halts when the input box is non-empty, you are typing, or multiple targets match.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                WorkbenchPolicySettings(runtime: runtime)
                GroupBox(L("系统行为", "System Behavior")) {
                    VStack(alignment: .leading, spacing: 14) {
                        Toggle(L("保持系统唤醒，允许显示器关闭", "Keep system awake, allow display sleep"), isOn: Binding(get: { runtime.awake }, set: { _ in runtime.toggleAwake() }))
                        Toggle(L("登录时启动", "Launch at login"), isOn: Binding(get: { atLogin }, set: changeLogin))
                        Toggle(L("始终在程序坞显示图标", "Always show icon in Dock"), isOn: $alwaysInDock).onChange(of: alwaysInDock) { DockPresence.apply(windowOpen: true) }
                        Text(L("默认只在工作台打开时显示程序坞图标，关闭工作台后只留在菜单栏。", "By default, shows in Dock only when the workbench window is open; stays in menu bar when closed.")).font(.caption).foregroundStyle(.secondary)
                        Text(L("不会自动解锁屏幕，不阻止合盖或手动睡眠。登录项需要的系统批准由你确认。", "Does not unlock screen or prevent lid-close / manual sleep. System login item permissions require your confirmation.")).font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox(L("关于", "About")) {
                    VStack(alignment: .leading, spacing: 12) {
                        Button(L("MCP 授权与逐操作审批", "MCP Authorization & Per-action Approvals")) { runtime.openWorkbench(.mcp) }
                        Text(L("版本 ", "Version ") + (Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "1.0.1")).font(.caption.monospaced())
                        Button(L("退出 LimitBreak", "Quit LimitBreak")) { NSApplication.shared.terminate(nil) }
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
