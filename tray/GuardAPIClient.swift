import Foundation

final class GuardNoRedirect: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
}

struct GuardAPIClient {
    private static let noRedirect = GuardNoRedirect()
    private static let session: URLSession = {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.connectionProxyDictionary = [:]
        configuration.urlCache = nil
        configuration.httpCookieStorage = nil
        configuration.httpShouldSetCookies = false
        return URLSession(configuration: configuration, delegate: noRedirect, delegateQueue: nil)
    }()
    let listen: String
    let token: String
    func request(_ path: String, body: Data? = nil, method: String? = nil) async throws -> Data {
        let verb = method ?? (body == nil ? "GET" : "POST")
        guard ["GET", "POST", "PATCH", "DELETE"].contains(verb), verb != "GET" || body == nil,
              path.hasPrefix("/api/"), !path.contains(".."), let url = URL(string: "http://\(listen)\(path)"),
              url.host == "127.0.0.1", url.user == nil, url.password == nil, url.fragment == nil,
              let port = url.port, (1...65535).contains(port), token.count >= 32,
              !token.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { throw RuntimeFailure("本地 API 地址或授权无效") }
        var request = URLRequest(url: url)
        request.httpMethod = verb
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.timeoutInterval = path == "/api/refresh" || path == "/api/mcp/requests/approve" ? 120 : path.hasSuffix("/locate") || path == "/api/desktop/probe" ? 30 : 10
        if let body { request.httpBody = body; request.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        let (bytes, response) = try await Self.session.bytes(for: request)
        let limit = 8 * 1024 * 1024
        guard response.expectedContentLength <= Int64(limit) else { throw RuntimeFailure("响应超过安全大小限制") }
        var data = Data()
        for try await byte in bytes {
            guard data.count < limit else { throw RuntimeFailure("响应超过安全大小限制") }
            data.append(byte)
        }
        guard let response = response as? HTTPURLResponse else { throw RuntimeFailure("本地服务响应无效") }
        guard (200..<300).contains(response.statusCode) else {
            // Only safe public error codes are mapped; raw response text and credentials never enter UI diagnostics.
            let object = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any]
            let error = object?["error"] as? [String: Any]
            let code = error?["code"] as? String ?? object?["code"] as? String ?? ""
            let messages = ["refresh_busy": "额度查询正在运行，请等待完成", "binding_rejected": "绑定未通过核验，请检查来源、模型与目标", "proxy_unavailable": "额度来源查询失败，查看来源诊断", "unsafe_binding": "自动恢复缺少完整目标或副作用授权", "invalid_route": "恢复来源或模型声明无效", "sources_failed": "未能读取来源健康状态", "probe_failed": "无法读取桌面环境，未执行任何发送", "bridge_unavailable": "未配置桌面检查器"]
            // These server messages are fixed Chinese texts or validated user errors; they never carry paths or secrets.
            let verbatim: Set<String> = ["session_refused", "session_not_found", "session_unavailable", "invalid_session", "cancel_failed", "invalid_json", "content_type", "unsupported", "invalid_policy"]
            if verbatim.contains(code), let text = object?["message"] as? String, !text.isEmpty, text.count <= 300,
               !text.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) { throw RuntimeFailure(text) }
            if response.statusCode == 401 { throw RuntimeFailure("本地访问授权失效，请重开应用") }
            if response.statusCode == 409 { throw RuntimeFailure("参数、策略摘要或会话代次已变化；请重新读取并由用户重新确认。没有自动重试。") }
            let controlMessages = ["control_forbidden": "授权不允许此目标或操作", "control_expired": "请求已到期，请重新读取，不自动续期", "control_limit": "授权或请求记录已达上限", "control_invalid": "控制字段无效", "control_unavailable": "结果无法核验；请读取请求记录，勿重复执行"]
            throw RuntimeFailure(messages[code] ?? controlMessages[code] ?? "操作未完成（HTTP \(response.statusCode)），未绕过安全门控")
        }
        guard response.value(forHTTPHeaderField: "Content-Type")?.lowercased().hasPrefix("application/json") == true else { throw RuntimeFailure("本地响应不是 JSON，结果未确认") }
        return data
    }
}
