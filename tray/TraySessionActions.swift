import Foundation

extension TrayRuntime {
    private func sessionPath(_ id: String, _ action: String = "") throws -> String {
        var allowed = CharacterSet.urlPathAllowed
        allowed.remove(charactersIn: "/?#%")
        guard !id.isEmpty, id.utf8.count <= 256, let encoded = id.addingPercentEncoding(withAllowedCharacters: allowed) else { throw RuntimeFailure("会话标识无效") }
        return "/api/sessions/" + encoded + (action.isEmpty ? "" : "/" + action)
    }
    func updateSession(_ session: TraySession, _ patch: TraySessionPatch) async -> Bool {
        if let problem = patch.problem { error = problem; return false }
        guard !patch.isEmpty else { notice = "没有修改"; return true }
        do {
            let response = try JSONDecoder().decode(TraySessionPatchResponse.self, from: await request(sessionPath(session.id), body: JSONEncoder().encode(patch), method: "PATCH"))
            guard response.ok, response.session.id == session.id else { throw RuntimeFailure("会话设置保存结果未确认，请刷新") }
            await refresh()
            notice = "已保存「\(session.displayTitle)」的托管设置"
            return true
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "会话设置未保存"; return false }
    }
    // Read-only locate: brings the target into view without writing a draft or sending.
    func locate(_ session: TraySession) async -> String {
        do {
            let response = try JSONDecoder().decode(TrayLocateResponse.self, from: await request(sessionPath(session.id, "locate"), body: Data("{}".utf8)))
            let detail = response.result.message?.isEmpty == false ? response.result.message! : (response.result.code ?? "")
            return response.ok && response.result.ok ? "已定位到会话" + (detail.isEmpty ? "" : "：" + detail) : "未能定位：" + (detail.isEmpty ? "原因未知" : detail)
        } catch { return (error as? RuntimeFailure)?.message ?? "定位请求未完成" }
    }
    // One explicit, user-confirmed delivery; it still passes the idle gate and hook confirmation.
    func testSend(_ session: TraySession, message: String) async -> String {
        let text = message.trimmingCharacters(in: .whitespaces)
        if !text.isEmpty, text.utf8.count > 4096 || text.contains("\n") || text.contains("\r") { return "测试文案须为单行且 ≤4096 字节" }
        do {
            let body = try JSONSerialization.data(withJSONObject: text.isEmpty ? [:] : ["message": text])
            let response = try JSONDecoder().decode(TrayTestSendResponse.self, from: await request(sessionPath(session.id, "test-send"), body: body))
            guard response.ok else { throw RuntimeFailure("测试发送未排队") }
            await refresh()
            return "已排队测试发送；下方「测试发送进度」会实时显示每一步"
        } catch { return (error as? RuntimeFailure)?.message ?? "测试发送未排队" }
    }
    // The most recent test send for this session, finished or not.
    func latestTest(for session: TraySession) -> TrayJob? { index.latestTest[session.id] }
    // Newest first, at most TraySnapshotIndex.eventLimit.
    func events(for session: TraySession) -> [TrayAudit] { index.events[session.id] ?? [] }
    func cancel(_ session: TraySession) async {
        do {
            _ = try await request(sessionPath(session.id, "cancel"), body: Data("{}".utf8))
            await refresh()
            notice = "已停止「\(session.displayTitle)」的待处理恢复"
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "停止未确认，请刷新后查看状态" }
    }
}
