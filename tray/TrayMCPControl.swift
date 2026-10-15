import Foundation

extension TrayRuntime {
    func controlGrants() async throws -> [GuardMCPGrant] {
        var grants: [GuardMCPGrant] = [], cursor = ""
        for _ in 0..<3 {
            let page = try JSONDecoder().decode(GuardMCPGrantPage.self, from: await request(controlPage("grants", cursor: cursor)))
            guard page.grants.count <= 100 else { throw RuntimeFailure("授权分页超限") }
            grants += page.grants
            let next = page.next_cursor ?? ""
            if next.isEmpty { return grants }
            guard next != cursor, grants.count <= 128 else { throw RuntimeFailure("授权分页异常，未标记读取完成") }
            cursor = next
        }
        throw RuntimeFailure("授权读取超过有界分页限制")
    }
    func controlRequests() async throws -> [GuardMCPRequest] {
        var requests: [GuardMCPRequest] = [], cursor = ""
        for _ in 0..<6 {
            let page = try JSONDecoder().decode(GuardMCPRequestPage.self, from: await request(controlPage("requests", cursor: cursor)))
            guard page.requests.count <= 100 else { throw RuntimeFailure("请求分页超限") }
            requests += page.requests
            let next = page.next_cursor ?? ""
            if next.isEmpty { return requests }
            guard next != cursor, requests.count <= 512 else { throw RuntimeFailure("请求分页异常，未标记读取完成") }
            cursor = next
        }
        throw RuntimeFailure("请求读取超过有界分页限制")
    }
    private func controlPage(_ collection: String, cursor: String) throws -> String {
        guard cursor.utf8.count <= 128 else { throw RuntimeFailure("控制分页游标无效") }
        var query = URLComponents()
        query.queryItems = [URLQueryItem(name: "limit", value: "100")]
        if !cursor.isEmpty { query.queryItems?.append(URLQueryItem(name: "cursor", value: cursor)) }
        return "/api/mcp/\(collection)?" + (query.percentEncodedQuery ?? "limit=100")
    }
    func revokeControlGrant(_ id: String) async throws {
        guard GuardMCPContract.validID(id) else { throw RuntimeFailure("授权标识无效") }
        let body = try JSONSerialization.data(withJSONObject: ["grant_id": id])
        let data = try await request("/api/mcp/grants", body: body, method: "DELETE")
        struct Response: Decodable { let ok: Bool }
        guard try JSONDecoder().decode(Response.self, from: data).ok else { throw RuntimeFailure("撤销结果未确认") }
    }
    func createGrantAndExport(scopes: Set<String>, targets: Set<String>, ttl: Int, to destination: URL) async throws -> String {
        guard desktopExecutionReady, localServiceFresh else { throw RuntimeFailure("自有后台或状态尚未确认，不能创建授权") }
        let body = try GuardMCPContract.createGrant(scopes: scopes, targets: targets, ttl: ttl)
        // No retry: the server may have issued a grant even when the response is lost.
        let data = try await request("/api/mcp/grants", body: body)
        let issued = try JSONDecoder().decode(GuardMCPIssuedGrant.self, from: data)
        guard GuardMCPContract.validID(issued.grant.id), Set(issued.grant.scopes) == scopes, Set(issued.grant.session_targets) == targets else { throw RuntimeFailure("签发授权元数据不一致，未导出；请读取授权记录") }
        do {
            let profile = GuardScopedProfile(daemon_url: daemonURL.absoluteString, access_token: issued.access_token)
            try profile.writeNew(to: destination)
            // The secret is not stored in runtime state, UI models, logs, clipboard or preferences.
            return issued.grant.id
        } catch {
            do {
                try await revokeControlGrant(issued.grant.id)
                throw RuntimeFailure("profile导出失败，此次授权已撤销；不保留或再次导出令牌，请重新创建")
            } catch let failure as RuntimeFailure where failure.message.hasPrefix("profile导出失败") { throw failure }
            catch { throw RuntimeFailure("profile导出失败，撤销未确认；请在授权列表手动撤销此次授权，勿重试签发") }
        }
    }
    func controlApprovalProblem(_ request: GuardMCPRequest) -> String? {
        guard desktopExecutionReady, localServiceFresh else { return "后台未就绪或快照过期，需要重新读取并确认" }
        guard request.confirmable(at: now) else { return "请求已到期、非待确认状态或操作参数未知" }
        if request.action.session_id != "daemon" {
            guard let session = snapshot?.sessions?.first(where: { $0.id == request.action.session_id }), let generation = session.generation,
                  generation == request.action.expected_generation else { return "精确目标缺失或generation已经变化，需要新请求" }
            if request.action.operation == "enable_auto_resume", request.action.params["enabled"]?.bool == true {
                guard session.binding?.owner == .guardOwner, session.binding?.native_auto_resume_disabled_acknowledged == true,
                      session.binding?.acknowledge_side_effects == true else { return "会话未由用户确认守护负责／官方恢复禁用／副作用；先在会话绑定页明确授权" }
            }
        }
        if let expected = request.action.policy_digest, expected != snapshot?.policy_digest { return "策略digest已变化，需要新请求；不会替换摘要重试" }
        return nil
    }
    // A scoped AI client's own acknowledge field is never an authorization source.
    func decideControlRequest(_ captured: GuardMCPRequest, approve: Bool, userConfirmed: Bool) async throws -> GuardMCPRequest {
        guard userConfirmed else { throw RuntimeFailure("必须由主应用用户核对并确认，AI自己ack无效") }
        if approve, let problem = controlApprovalProblem(captured) { throw RuntimeFailure(problem) }
        let body = try GuardMCPContract.decision(captured, approve: approve, now: Date())
        let response = try JSONDecoder().decode(GuardMCPRequestResponse.self, from: await request("/api/mcp/requests/approve", body: body))
        guard response.request.id == captured.id, response.request.digest == captured.digest,
              response.request.action.expected_generation == captured.action.expected_generation, response.request.state != "pending" else { throw RuntimeFailure("决定结果未确认；请重新读取请求记录，不重复执行") }
        await refresh()
        return response.request
    }
}
