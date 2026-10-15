import CryptoKit
import Darwin
import Foundation

@main
struct NativeContractsTest {
    static func rejects(_ action: () throws -> Void) {
        do { try action(); preconditionFailure("Expected rejection") } catch {}
    }
    static func main() throws {
        let now = Date()
        let token = String(repeating: "a", count: 64)
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let deadline = formatter.string(from: now.addingTimeInterval(20))
        let envelope: [String: Any] = ["protocol": 1, "request_id": String(repeating: "b", count: 32), "token": token, "operation": "probe", "deadline": deadline]
        func encode(_ object: [String: Any]) throws -> Data { try JSONSerialization.data(withJSONObject: object) }
        let request = try DesktopIPCRequest.decode(encode(envelope), expectedToken: token, now: now)
        precondition(request.operation == "probe")
        rejects { _ = try DesktopIPCRequest.decode(encode(envelope), expectedToken: String(repeating: "c", count: 64), now: now) }
        var invalid = envelope; invalid["unexpected"] = true
        rejects { _ = try DesktopIPCRequest.decode(encode(invalid), expectedToken: token, now: now) }
        invalid = envelope; invalid["deadline"] = formatter.string(from: now.addingTimeInterval(60))
        rejects { _ = try DesktopIPCRequest.decode(encode(invalid), expectedToken: token, now: now) }
        invalid = envelope; invalid["deadline"] = formatter.string(from: now.addingTimeInterval(-1))
        rejects { _ = try DesktopIPCRequest.decode(encode(invalid), expectedToken: token, now: now) }
        invalid = envelope; invalid["protocol"] = true
        rejects { _ = try DesktopIPCRequest.decode(encode(invalid), expectedToken: token, now: now) }
        rejects { try StrictIPCJSON.check(Data(#"{"protocol":1,"protocol":1}"#.utf8)) }
        rejects { try StrictIPCJSON.check(Data(#"{"binding":{"auto_resume":true,"auto_resume":false}}"#.utf8)) }
        rejects { try StrictIPCJSON.check(Data("{} {}".utf8)) }
        let hosted: [String: Any] = ["session_id": "s1", "kind": "code", "executor": "claude-desktop", "resume_owner": "auto",
                                     "deep_link": "claude://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab"]
        var locate = envelope; locate["operation"] = "locate"; locate["binding"] = hosted
        let located = try DesktopIPCRequest.decode(encode(locate), expectedToken: token, now: now)
        precondition(located.binding?.isDeepLinkTarget == true && located.binding?.hostSessionID == "local_0123abcd-0123-4567-89ab-0123456789ab")
        var forged = hosted; forged["deep_link"] = "claude://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab/../x"
        locate["binding"] = forged
        rejects { _ = try DesktopIPCRequest.decode(encode(locate), expectedToken: token, now: now) }
        forged = hosted; forged["executor"] = "terminal"
        locate["binding"] = forged
        rejects { _ = try DesktopIPCRequest.decode(encode(locate), expectedToken: token, now: now) }
        locate["binding"] = hosted; locate["message"] = "继续"
        rejects { _ = try DesktopIPCRequest.decode(encode(locate), expectedToken: token, now: now) }
        let message = "继续"
        let hash = SHA256.hash(data: Data(message.utf8)).map { String(format: "%02x", $0) }.joined()
        var resume = envelope; resume["operation"] = "resume"; resume["binding"] = hosted; resume["message"] = message
        resume["generation"] = 2; resume["lease_id"] = "owner/job"; resume["message_hash"] = hash
        _ = try DesktopIPCRequest.decode(encode(resume), expectedToken: token, now: now)
        var manual = hosted; manual["resume_owner"] = "manual"; resume["binding"] = manual
        rejects { _ = try DesktopIPCRequest.decode(encode(resume), expectedToken: token, now: now) }
        var legacy = hosted; legacy["executor"] = nil; resume["binding"] = legacy
        rejects { _ = try DesktopIPCRequest.decode(encode(resume), expectedToken: token, now: now) }
        // A wake carries the send's lease and attention settings but never the message.
        var wake = envelope; wake["operation"] = "wake"; wake["binding"] = hosted
        wake["generation"] = 2; wake["lease_id"] = "owner/job"; wake["message_hash"] = hash; wake["quiet_seconds"] = 120; wake["restore_focus"] = true
        let woken = try DesktopIPCRequest.decode(encode(wake), expectedToken: token, now: now)
        precondition(woken.quiet_seconds == 120 && woken.restore_focus == true)
        var wakeWithText = wake; wakeWithText["message"] = message
        rejects { _ = try DesktopIPCRequest.decode(encode(wakeWithText), expectedToken: token, now: now) }
        var unleased = wake; unleased["lease_id"] = nil
        rejects { _ = try DesktopIPCRequest.decode(encode(unleased), expectedToken: token, now: now) }
        var tooQuiet = wake; tooQuiet["quiet_seconds"] = 3601
        rejects { _ = try DesktopIPCRequest.decode(encode(tooQuiet), expectedToken: token, now: now) }
        var plainWake = wake; plainWake["binding"] = legacy
        rejects { _ = try DesktopIPCRequest.decode(encode(plainWake), expectedToken: token, now: now) }
        let sessionData = Data(#"{"id":"session","title":"specific-target","cwd":"/isolated/project","model":"opus","state":"COOLDOWN","reason":"quota","kind":"code","generation":1,"binding":{"auto_resume":true},"host":{"mode":"legacy","ready":false,"problem":"旧绑定"}}"#.utf8)
        let session = try JSONDecoder().decode(TraySession.self, from: sessionData)
        precondition(session.binding?.owner == .unknown && session.binding?.guardAuthorized == false && session.host?.mode == "legacy")
        precondition(TraySessionPatch().isEmpty && TraySessionPatch().problem == nil)
        precondition(TraySessionPatch(host_mode: "hosted", continue_message: "", pool_override: [], target_override: "", priority: 0).problem == nil)
        precondition(TraySessionPatch(host_mode: "auto").problem != nil)
        precondition(TraySessionPatch(continue_message: "第一行\n第二行").problem != nil)
        precondition(TraySessionPatch(continue_message: "   ").problem != nil)
        precondition(TraySessionPatch(priority: 101).problem != nil)
        precondition(TraySessionPatch(pool_override: [""]).problem != nil)
        precondition(TraySessionPatch.validTarget("") && TraySessionPatch.validTarget("desktop"))
        precondition(!TraySessionPatch.validTarget("terminal:1") && !TraySessionPatch.validTarget("Desktop") && !TraySessionPatch.validTarget("tmux:1"))
        let partial = try JSONSerialization.jsonObject(with: JSONEncoder().encode(TraySessionPatch(continue_message: ""))) as! [String: Any]
        precondition(Set(partial.keys) == ["continue_message"], "Unchanged session fields must not be sent")
        let observation = GuardDesktopObservation(["bridge_checked": .bool(true), "accessibility_granted": .bool(true), "screen_locked": .bool(false)], now: now)
        precondition(!observation.checked && observation.permission == .unknown && observation.screenLocked == nil)
        let actual = GuardDesktopObservation(["bridge_checked": .bool(true), "desktop_checked_at": .string(formatter.string(from: now.addingTimeInterval(-1))), "accessibility_granted": .bool(true), "accessibility_state": .string("granted"), "screen_locked": .bool(false), "desktop_code": .string("ready")], now: now)
        precondition(actual.checked && actual.permission == .granted && actual.screenLocked == false)
        let digest = String(repeating: "d", count: 64)
        let patch = try GuardPolicyPatch.data(["max_running": .number(3)], digest: digest)
        let object = try JSONSerialization.jsonObject(with: patch) as! [String: Any]
        precondition(Set(object.keys) == ["policy_digest", "patch"])
        rejects { _ = try GuardPolicyPatch.data(["user_idle_seconds": .number(0)], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["token": .string("not-exportable")], digest: digest) }
        precondition(GuardPolicyPatch.controlNumericKeys.isSubset(of: GuardPolicyPatch.numericKeys) && !GuardPolicyPatch.controlNumericKeys.contains("native_grace_seconds"))
        _ = try GuardPolicyPatch.data(["default_host_mode": .string("monitor"), "automation_consent": .bool(true), "native_grace_seconds": .number(90),
                                       "model_aliases": .object(["claude-opus-4-8": .string("opus")])], digest: digest)
        rejects { _ = try GuardPolicyPatch.data(["default_host_mode": .string("auto")], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["automation_consent": .string("yes")], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["native_grace_seconds": .number(0)], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["model_aliases": .object(["Claude-Opus": .string("opus")])], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["model_aliases": .object(["opus": .string("")])], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["continue_message": .string("a\nb")], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["paused": .bool(false)], digest: "short") }
        precondition(GuardPolicyPatch.normalizeModel(" Claude-Opus-4-8[1m] ") == "claude-opus-4-8")
        precondition(GuardPolicyPatch.confirms(["automation_consent": .bool(true)], policy: ["automation_consent_at": .string("2026-10-12T00:00:00Z")]))
        precondition(GuardPolicyPatch.confirms(["automation_consent": .bool(false)], policy: [:]))
        precondition(!GuardPolicyPatch.confirms(["automation_consent": .bool(true)], policy: [:]))
        precondition(GuardPolicyPatch.confirms(["default_host_mode": .string("")], policy: [:]))
        precondition(GuardPolicyPatch.confirms(["model_aliases": .object([:])], policy: [:]))
        precondition(!GuardPolicyPatch.confirms(["max_running": .number(3)], policy: ["max_running": .number(2)]))
        _ = try GuardPolicyPatch.data(["direct_delivery": .string("off")], digest: digest)
        rejects { _ = try GuardPolicyPatch.data(["direct_delivery": .string("always")], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["direct_delivery": .bool(true)], digest: digest) }
        precondition(GuardPolicyPatch.confirms(["direct_delivery": .string("auto")], policy: [:]))
        precondition(GuardPolicyPatch.confirms(["direct_delivery": .string("off")], policy: ["direct_delivery": .string("off")]))
        precondition(!GuardPolicyPatch.confirms(["direct_delivery": .string("off")], policy: [:]))
        _ = try GuardPolicyPatch.data(["reset_fallback": .string("off")], digest: digest)
        rejects { _ = try GuardPolicyPatch.data(["reset_fallback": .string("always")], digest: digest) }
        precondition(GuardPolicyPatch.confirms(["reset_fallback": .string("auto")], policy: [:]))
        precondition(!GuardPolicyPatch.confirms(["reset_fallback": .string("off")], policy: [:]))
        _ = try GuardPolicyPatch.data(["restore_focus": .string("off"), "claude_quiet": .string("auto"), "claude_quiet_seconds": .number(300)], digest: digest)
        rejects { _ = try GuardPolicyPatch.data(["claude_quiet": .bool(true)], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["claude_quiet_seconds": .number(0)], digest: digest) }
        rejects { _ = try GuardPolicyPatch.data(["claude_quiet_seconds": .number(3601)], digest: digest) }
        precondition(GuardPolicyPatch.confirms(["restore_focus": .string("auto"), "claude_quiet": .string("auto")], policy: [:]))
        precondition(!GuardPolicyPatch.confirms(["claude_quiet": .string("off")], policy: [:]))
        rejects { _ = try GuardMCPContract.createGrant(scopes: ["read", "direct_resume"], targets: ["daemon"], ttl: 60) }
        rejects { _ = try GuardMCPContract.createGrant(scopes: ["read"], targets: ["*"], ttl: 60) }
        _ = try GuardMCPContract.createGrant(scopes: ["read", "action:settingsPatch"], targets: ["daemon"], ttl: 60)
        let oldHooks = try GuardHookPlan.fragment(engineVersion: "2.1.233", command: "owned-command")
        precondition(oldHooks["Notification"] == nil)
        let hooks = try GuardHookPlan.fragment(engineVersion: "2.1.234", command: "owned-command")
        precondition(hooks["Notification"]?.first?["matcher"] as? String == "quota_auto_resume_fired|quota_auto_resume_stale|quota_auto_resume_disabled")
        precondition(hooks["PostModelSwitch"] == nil)
        let newerHooks = try GuardHookPlan.fragment(engineVersion: "2.1.251", command: "owned-command")
        precondition(newerHooks["PostModelSwitch"] != nil)
        rejects { _ = try GuardHookPlan.fragment(engineVersion: "unknown", command: "owned-command") }
        guard let temp = ProcessInfo.processInfo.environment["TMPDIR"], !temp.isEmpty else { throw RuntimeFailure("Tests require TMPDIR") }
        let directory = URL(fileURLWithPath: temp).appendingPathComponent("qso-native-contracts-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: directory) }
        let profile = GuardScopedProfile(daemon_url: "http://127.0.0.1:8318", access_token: "qso_scope_" + token)
        let target = directory.appendingPathComponent("profile.json")
        try profile.writeNew(to: target)
        let original = try Data(contentsOf: target)
        let attributes = try FileManager.default.attributesOfItem(atPath: target.path)
        precondition((attributes[.posixPermissions] as? NSNumber)?.intValue == 0o600)
        rejects { try profile.writeNew(to: target) }
        let unchanged = try Data(contentsOf: target)
        precondition(unchanged == original)
        let link = directory.appendingPathComponent("link.json")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: target)
        rejects { try profile.writeNew(to: link) }
        rejects { try GuardScopedProfile(daemon_url: "https://external.invalid", access_token: "qso_scope_" + token).validate() }
        rejects { try GuardScopedProfile(daemon_url: "http://127.0.0.1:8318", access_token: token).validate() }
        print("原生IPC认证、期限、重复字段、恢复所有权、真实检测时间、CAS、托管策略与会话补丁校验、Hook版本门控与0600单次导出契约通过；未操作桌面或真实会话。")
    }
}
