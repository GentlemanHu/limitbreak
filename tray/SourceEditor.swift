import SwiftUI

struct SourceEditor: View {
    @ObservedObject var runtime: TrayRuntime
    let original: [String: Any]
    let close: () -> Void
    @State private var id = ""
    @State private var type = "readonly-http"
    @State private var url = ""
    @State private var credentialEnv = ""
    @State private var credential = ""
    @State private var poll = "180"
    @State private var busy = false
    @State private var error = ""
    @State private var consent = false
    private var existing: Bool { original["id"] != nil }
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Text(existing ? "配置额度来源" : "添加额度来源").font(.title2.bold())
            Text("保存会暂停自动恢复并重启自有后台。请核对来源端点、账号及绑定后再开启恢复。").foregroundStyle(.secondary)
            Form {
                TextField("来源标识", text: $id).disabled(existing || busy)
                Picker("类型", selection: $type) { Text("只读 HTTP 服务").tag("readonly-http"); Text("CLIProxyAPI 管理接口").tag("cliproxyapi") }.disabled(existing || busy)
                TextField("服务地址", text: $url).disabled(busy)
                TextField("凭据环境变量名", text: $credentialEnv).disabled(busy)
                SecureField(existing ? "新凭据（留空保留）" : "凭据（可选，存入钥匙串）", text: $credential).disabled(busy)
                TextField("轮询秒数（30–86400）", text: $poll).disabled(busy)
            }.textFieldStyle(.roundedBorder)
            Text(type == "readonly-http" ? "服务必须返回 normalized-v1。仅 GET，不跟随重定向；默认响应上限 1 MiB。HTTPS 或 IP 字面量回环 HTTP，不接受 URL 中的密钥。" : "明确提供该来源的管理凭据环境变量。密钥通过钥匙串注入自有子进程，不写入来源清单。已有查询配置会原样保留。").font(.caption).foregroundStyle(.secondary)
            Text("应用不扫描其他工具凭据。填写凭据即可图形化配置，不需要终端 export。留空只保留相同来源、地址、类型与变量名的钥匙串凭据；更换这些字段需要重新输入，或使用你明确配置的环境变量。").font(.caption).foregroundStyle(.secondary)
            if !error.isEmpty { Text(error).foregroundStyle(.orange).textSelection(.enabled) }
            HStack { Button("返回", action: close).disabled(busy); Spacer(); Button(busy ? "保存中…" : "校验并保存") { consent = true }.buttonStyle(.borderedProminent).disabled(busy || id.isEmpty || url.isEmpty) }
        }.padding(26).frame(width: 610)
            .onAppear {
                id = original["id"] as? String ?? ""
                type = original["type"] as? String ?? "readonly-http"
                url = original["url"] as? String ?? ""
                credentialEnv = original["credential_env"] as? String ?? ""
                poll = String(original["poll_seconds"] as? Int ?? 180)
            }
            .alert("暂停恢复并保存来源？", isPresented: $consent) {
                Button("返回", role: .cancel) {}
                Button("暂停并保存") { Task { await save() } }
            } message: {
                Text("凭据将仅供你明确配置的服务使用：\(url)。请确认该地址可信。更换地址后不继承原地址的钥匙串凭据。" + (existing ? "会保留其他来源与查询字段；不会自动重绑会话或宣称路由已切换。" : "第一个显式来源会取代兼容默认来源列表，不自动复制默认连接或密钥。若需要默认 CPA，请同时显式配置 cpa-default。"))
            }
    }
    private func save() async {
        guard !busy else { return }; busy = true; error = ""; defer { busy = false }
        do {
            let identifier = id.trimmingCharacters(in: .whitespacesAndNewlines)
            guard identifier.range(of: "^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$", options: .regularExpression) != nil else { throw RuntimeFailure("来源标识需为 1–64 位字母、数字、点、下划线或短横线，首位为字母数字") }
            let configurations = try runtime.sourceConfigurations()
            if !existing && configurations.contains(where: { $0["id"] as? String == identifier }) { throw RuntimeFailure("来源标识已存在，请选择另一个标识") }
            guard let interval = Int(poll), (30...86400).contains(interval) else { throw RuntimeFailure("轮询间隔需为 30–86400 的整数") }
            var name = credentialEnv.trimmingCharacters(in: .whitespacesAndNewlines)
            if name.isEmpty && (!credential.isEmpty || type == "cliproxyapi") { name = "QSO_SOURCE_" + identifier.uppercased().replacingOccurrences(of: "-", with: "_").replacingOccurrences(of: ".", with: "_") }
            if !name.isEmpty {
                guard name.range(of: "^[A-Za-z_][A-Za-z0-9_]*$", options: .regularExpression) != nil else { throw RuntimeFailure("凭据环境变量名格式无效") }
                guard !["PATH", "HOME", "TMPDIR", "DYLD_INSERT_LIBRARIES", "DYLD_LIBRARY_PATH", "LD_PRELOAD"].contains(name), !name.hasPrefix("DYLD_"), !name.hasPrefix("LD_") else { throw RuntimeFailure("不能将系统控制变量用作凭据名称") }
                guard !configurations.contains(where: { $0["id"] as? String != identifier && $0["credential_env"] as? String == name }) else { throw RuntimeFailure("不同来源不能共享同一个凭据变量名，以免错用凭据") }
            }
            var value = original
            value["id"] = identifier; value["type"] = type; value["url"] = url.trimmingCharacters(in: .whitespacesAndNewlines); value["poll_seconds"] = interval
            value["credential_env"] = name.isEmpty ? nil : name
            if type == "readonly-http" { value["schema"] = "normalized-v1" }
            let secret = credential; credential = ""
            try await runtime.saveSource(value, credential: secret)
            close()
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "保存未完成；恢复保持暂停，请检查后台状态。" }
    }
}
