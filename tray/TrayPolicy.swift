import Foundation

extension TrayRuntime {
    // No replacement policy or retry against a newly fetched digest without consent.
    func patchPolicy(_ changes: [String: JSONValue], expectedDigest: String?) async throws -> GuardPolicyPatchResponse {
        let payload = try GuardPolicyPatch.data(changes, digest: expectedDigest)
        let response = try JSONDecoder().decode(GuardPolicyPatchResponse.self, from: await request("/api/policy/patch", body: payload))
        guard response.ok, response.policy_digest.utf8.count == 64 else { throw RuntimeFailure("策略保存结果未确认，请重新读取；没有重试") }
        guard GuardPolicyPatch.confirms(changes, policy: response.policy) else { throw RuntimeFailure("策略响应未包含已确认的修改，请重新读取；没有重试") }
        return response
    }
}
