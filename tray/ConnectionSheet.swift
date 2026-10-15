import SwiftUI

// One place for the CLIProxyAPI connection: current state first, then the editable
// address/key. A saved key is never shown; leaving the field empty keeps it.
struct ConnectionSheet: View {
    @ObservedObject var runtime: TrayRuntime
    let close: () -> Void
    @State private var url = ""
    @State private var key = ""
    @State private var explicitSources = false
    @State private var rechecking = false
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("CLIProxyAPI 连接").font(.title3.bold())
            status
            if explicitSources {
                Text("当前使用「来源与适配」中的多来源配置；地址和密钥请在那里按来源修改。").font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                HStack {
                    Button("关闭", action: close)
                    Spacer()
                    Button("打开来源与适配") { close(); runtime.openWorkbench(.sources) }.buttonStyle(.borderedProminent)
                }
            } else {
                form
            }
        }.padding(24).frame(width: 460)
            .onAppear {
                url = runtime.proxyURL
                explicitSources = (try? runtime.sourceConfigurations())?.isEmpty == false
            }
    }

    @ViewBuilder private var status: some View {
        let source = runtime.proxySource
        HStack(alignment: .top, spacing: 10) {
            switch runtime.sourcePhase {
            case .connected:
                Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                VStack(alignment: .leading, spacing: 3) {
                    Text("已连接 · " + runtime.proxyURL).lineLimit(1).truncationMode(.middle)
                    Text(connectedDetail(source)).font(.caption).foregroundStyle(.secondary)
                }
            case .starting, .checking:
                ProgressView().controlSize(.small)
                Text("正在读取额度来源…（已等待 \(runtime.secondsSinceLaunch) 秒）")
            case .failed:
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                Text(runtime.sourceProblem).fixedSize(horizontal: false, vertical: true)
            case .unconfigured:
                Image(systemName: "link.badge.plus").foregroundStyle(.secondary)
                Text("尚未连接。填写本机 CLIProxyAPI 地址和管理密钥。")
            }
            Spacer(minLength: 0)
        }.font(.callout).padding(12).frame(maxWidth: .infinity, alignment: .leading)
            .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 10))
    }

    private func connectedDetail(_ source: GuardSource?) -> String {
        var parts = ["\(source?.account_count ?? runtime.snapshot?.accounts?.count ?? 0) 个账号"]
        if let checked = source.flatMap({ trayDate($0.checked_at) }) { parts.append("上次读取 " + checked.formatted(date: .omitted, time: .standard)) }
        return parts.joined(separator: " · ")
    }

    private var savedKey: Bool { runtime.hasSavedManagementKey }
    private var trimmedKey: String { key.trimmingCharacters(in: .whitespacesAndNewlines) }
    private var urlChanged: Bool { url.trimmingCharacters(in: .whitespaces) != runtime.proxyURL }
    // Re-saving an unchanged address with the saved key would only restart the backend; "重新读取" covers that.
    private var canSave: Bool { !runtime.connecting && (trimmedKey.isEmpty ? savedKey && urlChanged : true) && !url.trimmingCharacters(in: .whitespaces).isEmpty }

    @ViewBuilder private var form: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("代理地址").font(.caption).foregroundStyle(.secondary)
            TextField("http://127.0.0.1:8317", text: $url).textFieldStyle(.roundedBorder)
        }
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("管理密钥").font(.caption).foregroundStyle(.secondary)
                if savedKey { Label("已保存", systemImage: "lock.fill").font(.caption).foregroundStyle(.green) }
            }
            SecureField(savedKey ? "留空则保留已保存的密钥" : "粘贴 CLIProxyAPI 管理密钥", text: $key).textFieldStyle(.roundedBorder)
            Text("密钥只写入本用户私有文件，不会回显，也不进入菜单栏、日志、插件清单或网页地址。").font(.caption2).foregroundStyle(.secondary)
        }
        if !runtime.error.isEmpty { Text(runtime.error).font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true) }
        HStack {
            Button("关闭", action: close).disabled(runtime.connecting).keyboardShortcut(.cancelAction)
            Spacer()
            if runtime.configured {
                Button(rechecking ? "读取中…" : "重新读取额度") {
                    rechecking = true
                    Task { await runtime.refreshQuota(); rechecking = false }
                }.disabled(rechecking || runtime.connecting || !runtime.daemonRunning)
                    .help("不改设置，立即向 CLIProxyAPI 重新读取一次额度")
            }
            Button(runtime.connecting ? "连接中…" : runtime.configured ? "保存并重新连接" : "连接并保存") {
                let secret = trimmedKey; key = ""
                Task {
                    await runtime.connect(url: url, key: secret)
                    if runtime.proxyConnected { runtime.notice = "已连接 CLIProxyAPI"; close() }
                }
            }.buttonStyle(.borderedProminent).disabled(!canSave).keyboardShortcut(.defaultAction)
        }
    }
}
