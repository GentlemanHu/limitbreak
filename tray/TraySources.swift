import Foundation

extension TrayRuntime {
    func sourceConfigurations() throws -> [[String: Any]] { try readConfig()["sources"] as? [[String: Any]] ?? [] }

    func saveSource(_ source: [String: Any], credential: String) async throws {
        guard !connecting else { throw RuntimeFailure("其他连接配置正在保存，请稍后再试") }
        connecting = true; defer { connecting = false }
        guard let id = source["id"] as? String else { throw RuntimeFailure("来源标识缺失") }
        var configuration = try readConfig()
        var sources = configuration["sources"] as? [[String: Any]] ?? []
        if let index = sources.firstIndex(where: { $0["id"] as? String == id }) { sources[index] = source }
        else { sources.append(source) }
        configuration["sources"] = sources
        try await validateConfiguration(configuration)
        let secret = credential.trimmingCharacters(in: .whitespacesAndNewlines)
        guard secret.utf8.count <= 4096, !secret.contains("\n"), !secret.contains("\r") else { throw RuntimeFailure("凭据格式无效") }
        try await pauseForConfiguration()
        if !secret.isEmpty {
            guard let type = source["type"] as? String, let url = source["url"] as? String, let name = source["credential_env"] as? String, !name.isEmpty else { throw RuntimeFailure("保存凭据需要明确来源地址、类型与环境变量引用") }
            let identity = GuardSourceCredentials.identity(sourceID: id, type: type, url: url, environment: name)
            try GuardSourceCredentials.save(secret, for: identity)
        }
        try await applyConfiguration(configuration)
        configured = true
    }

    func removeSource(_ id: String) async throws {
        guard !connecting else { throw RuntimeFailure("其他连接配置正在保存，请稍后再试") }
        connecting = true; defer { connecting = false }
        var configuration = try readConfig()
        var sources = configuration["sources"] as? [[String: Any]] ?? []
        guard sources.contains(where: { $0["id"] as? String == id }) else { throw RuntimeFailure("这是兼容默认连接，请在连接设置中修改") }
        guard sources.count > 1 else { throw RuntimeFailure("不能移除最后一个显式来源：空列表会重新启用兼容默认连接。请先配置替代来源。") }
        sources.removeAll { $0["id"] as? String == id }
        configuration["sources"] = sources
        try await validateConfiguration(configuration)
        try await applyConfiguration(configuration)
    }

    private func validateConfiguration(_ configuration: [String: Any]) async throws {
        let temporary = dataDirectory.appendingPathComponent(".validate-\(UUID().uuidString).json")
        let bytes = try JSONSerialization.data(withJSONObject: configuration, options: [.sortedKeys])
        try GuardPrivateStorage.write(bytes, to: temporary)
        defer { try? FileManager.default.removeItem(at: temporary) }
        let process = Process()
        process.executableURL = resources.appendingPathComponent("bin/orchestrator")
        process.arguments = ["validate", "--config", temporary.path]
        process.environment = ["PATH": "/usr/bin:/bin"]
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        try process.run()
        for _ in 0..<100 {
            if !process.isRunning { break }
            try await Task.sleep(nanoseconds: 50_000_000)
        }
        guard !process.isRunning else { process.terminate(); throw RuntimeFailure("配置校验超时；原配置未修改") }
        guard process.terminationStatus == 0 else { throw RuntimeFailure("配置校验未通过；检查来源标识、地址、凭据环境变量名或类型专属字段。原配置未修改。") }
    }
}
