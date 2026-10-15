import Foundation

@main
struct TrayModelsTest {
    static func main() throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let policy: [String: JSONValue] = ["max_running": .number(2), "paused": .bool(true), "continue_message": .string("继续")]
        let encoded = try encoder.encode(policy)
        let text = String(data: encoded, encoding: .utf8)!
        precondition(text.contains("\"max_running\":2,"), "Integer policy must remain valid for the Go API")
        let decoded = try JSONDecoder().decode([String: JSONValue].self, from: encoded)
        precondition(decoded["paused"]?.bool == true)
        precondition(decoded["max_running"]?.number == 2)
        precondition(decoded["continue_message"]?.string == "继续")
        precondition(trayDate("2026-10-12T00:00:00.123Z") != nil)
        precondition(trayDate("2026-10-12T00:00:00Z") != nil)
        precondition(trayDate(nil) == nil)
        precondition(trayDuration(3600) == "01:00:00")
        precondition(trayDuration(-2) == "00:00:00")
        precondition(trayDuration(86400) == "1天 00:00")
        precondition(trayState("COOLDOWN") == "等待额度恢复")
        let ref = TrayResourceRef(account_id: "account", group_id: "group", window_id: "window")
        let refs = try encoder.encode([ref, ref])
        let roundtrip = try JSONDecoder().decode([TrayResourceRef].self, from: refs)
        precondition(Set(roundtrip).count == 1)
        let snapshot = Data(#"{"accounts":[],"sessions":[{"id":"session","title":"title","cwd":"/project","model":"","state":"RUNNING","reason":"hook","kind":"code","monitor":{"mode":"all","state":"unknown","resources":[]}}],"jobs":[],"policy":{},"capabilities":{}}"#.utf8)
        let state = try JSONDecoder().decode(TraySnapshot.self, from: snapshot)
        precondition(state.sessions?.first?.monitor?.mode == "all")
        precondition(state.sessions?.first?.kind == "code")
        let sourceJSON = Data(#"{"sources":[{"id":"cpa-default","type":"cliproxyapi","state":"error","connected":false,"checked_at":"2026-10-12T00:00:00Z","account_count":1}]}"#.utf8)
        let sources = try JSONDecoder().decode(GuardSourceCatalog.self, from: sourceJSON)
        precondition(sources.sources.first?.connected == false)
        let adapterJSON = Data(#"{"adapters":[{"id":"ccswitch","name":"CCSwitch","kind":"quota_source","implemented":false,"read_only":true,"capabilities":[],"limitations":[]}]}"#.utf8)
        let adapters = try JSONDecoder().decode(GuardAdapterCatalog.self, from: adapterJSON)
        precondition(adapters.adapters.first?.implemented == false)
        let hosted = Data(#"{"policy":{"paused":false,"automation_consent_at":"2026-10-12T00:00:00Z","default_host_mode":"monitor","model_aliases":{"claude-opus-4-8":"claude-opus-4-8-20260101"}},"capabilities":{},"sessions":[{"id":"s1","title":"","cwd":"/work/app","model":"claude-opus-4-8","state":"COOLDOWN","reason":"rate limit","reason_text":"等待额度恢复","host_mode":"hosted","continue_message":"继续工作","pool_override":["a1"],"target_override":"desktop","priority":10,"native_resume":"stale","origin":{"app":"claude-desktop","host_session_id":"local_0123abcd-0123-4567-89ab-0123456789ab","engine_version":"2.1.197"},"pool":{"model":"claude-opus-4-8","account_ids":["a1"],"basis":"manual","inferred":false},"host":{"mode":"hosted","executor":"claude-desktop","target":"claude://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab","ready":true}}],"jobs":[{"id":"j1","session_id":"s1","state":"COOLDOWN","not_before":"2026-10-12T01:00:00Z","kind":"resume","account_id":"a1","attempts":1,"reset_hint":"2026-10-12T00:59:00Z"},{"session_id":"s1","state":"COMPLETED","not_before":"2026-10-11T00:00:00Z"}],"audit":[{"id":7,"time":"2026-10-12T00:00:00Z","kind":"dispatch","session_id":"s1","message":"sent"}]}"#.utf8)
        let four = try JSONDecoder().decode(TraySnapshot.self, from: hosted)
        precondition(!four.paused && four.automationConsented && four.defaultHostMode == "monitor")
        precondition(four.modelAliases["claude-opus-4-8"] == "claude-opus-4-8-20260101")
        let s1 = four.sessions!.first!
        precondition(s1.displayTitle == "app" && s1.explanation == "等待额度恢复")
        precondition(s1.origin?.appTitle == "Claude 桌面 Code" && s1.pool?.basisTitle == "手动指定" && s1.host?.ready == true && s1.host?.executorTitle == "Claude 桌面")
        let terminal = try JSONDecoder().decode(TrayOrigin.self, from: Data(#"{"app":"terminal","claude_pid":700,"exec_path":"/Users/u/.local/bin/claude","tmux_pane":"%1","detached":true}"#.utf8))
        precondition(terminal.appTitle == "终端" && terminal.tmux_pane == "%1" && terminal.detached == true && terminal.exec_path == "/Users/u/.local/bin/claude")
        let terminalHost = try JSONDecoder().decode(TrayHost.self, from: Data(#"{"mode":"hosted","executor":"claude-terminal","target":"claude-cli:x","ready":true}"#.utf8))
        precondition(terminalHost.executorTitle == "终端 claude")
        let client = try JSONDecoder().decode(TrayOrigin.self, from: Data(#"{"app":"client","client":"codex"}"#.utf8))
        let clientHost = try JSONDecoder().decode(TrayHost.self, from: Data(#"{"mode":"hosted","executor":"client:codex","ready":true}"#.utf8))
        precondition(client.appTitle == "客户端 codex" && clientHost.executorTitle == "客户端命令 codex")
        precondition(s1.priority == 10 && s1.pool_override == ["a1"] && s1.target_override == "desktop")
        precondition(four.jobs!.filter(\.active).count == 1 && four.jobs![1].stableID == "s1/2026-10-11T00:00:00Z")
        precondition(four.audit?.first?.kind == "dispatch")
        precondition(TrayHostMode.title("legacy") == "旧版绑定" && TrayHostMode(rawValue: "") == .follow)
        let unconsented = try JSONDecoder().decode(TraySnapshot.self, from: Data(#"{"policy":{},"capabilities":{}}"#.utf8))
        precondition(unconsented.paused && !unconsented.automationConsented && unconsented.modelAliases.isEmpty && unconsented.signals == nil)
        let signalJSON = Data(#"{"policy":{},"capabilities":{},"signals":[{"id":"codex-resets","type":"codex-resets","name":"Codex Resets","provider":"codex","enabled":true,"state":"ok","next_poll_at":"2026-10-12T00:01:00Z","latest":{"id":"r1","source":"codex-resets","provider":"codex","kind":"reset_executed","at":"2026-10-12T00:00:00Z","received_at":"2026-10-12T00:00:05Z"},"watch":{"level":"elevated","chance_percent":40},"attribution":"Data from Codex Resets","attribution_url":"https://codex-resets.com"}]}"#.utf8)
        let signal = try JSONDecoder().decode(TraySnapshot.self, from: signalJSON).signals!.first!
        precondition(signal.latest?.id == "r1" && signal.watch?.chance_percent == 40 && signal.attribution_url == "https://codex-resets.com" && signal.scheduled == nil)
        print("原生数据契约、来源能力、会话托管字段、组合资源与倒计时验证通过。")
    }
}
