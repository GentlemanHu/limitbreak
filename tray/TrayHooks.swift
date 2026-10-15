import AppKit
import Foundation

struct GuardHookState: Equatable {
    var events: [String] = []
    var stalePaths = 0
    var blockedByPolicy = false
    var unreadable = false
}

extension TrayRuntime {
    private var claudeSettingsURL: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".claude/settings.json")
    }
    private static func shellQuote(_ value: String) -> String { "'" + value.replacingOccurrences(of: "'", with: "'\"'\"'") + "'" }
    // Hooks call a fixed entry in the data directory, not this build's bundle path, so upgrades
    // and moving the app never require editing ~/.claude/settings.json again.
    var hookEntryURL: URL { dataDirectory.appendingPathComponent("bin/orchestrator") }
    var hookCommand: String {
        Self.shellQuote(hookEntryURL.path) + " hook --config " + Self.shellQuote(configURL.path)
    }
    // Repoints the fixed entry at the build that is running now; atomic, so a hook firing meanwhile sees old or new.
    func refreshHookEntry() throws {
        let manager = FileManager.default
        let target = resources.appendingPathComponent("bin/orchestrator").path
        if (try? manager.destinationOfSymbolicLink(atPath: hookEntryURL.path)) == target { return }
        let directory = hookEntryURL.deletingLastPathComponent()
        try manager.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let staged = directory.appendingPathComponent(".orchestrator-\(UUID().uuidString)")
        try manager.createSymbolicLink(atPath: staged.path, withDestinationPath: target)
        guard rename(staged.path, hookEntryURL.path) == 0 else {
            try? manager.removeItem(at: staged)
            throw RuntimeFailure("无法更新稳定 Hook 入口（\(hookEntryURL.path)）；会话事件可能仍指向旧版本")
        }
    }
    // A hook command of another build of this app writing to the same data directory (an earlier version or path).
    private func isSupersededHook(_ text: String) -> Bool {
        text != hookCommand && text.hasPrefix("'/") && text.hasSuffix("/bin/orchestrator' hook --config " + Self.shellQuote(configURL.path))
    }
    private func supersededHookCount() -> Int {
        guard let data = try? Data(contentsOf: claudeSettingsURL),
              let config = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { return 0 }
        var count = 0
        for (_, value) in config["hooks"] as? [String: Any] ?? [:] {
            for group in value as? [[String: Any]] ?? [] {
                count += (group["hooks"] as? [[String: Any]] ?? []).filter { isSupersededHook($0["command"] as? String ?? "") }.count
            }
        }
        return count
    }
    // Highest embedded Code engine installed by the Claude desktop app; directory names are versions.
    func detectedEngineVersion() -> String? {
        let root = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/Claude/claude-code", isDirectory: true)
        let names = (try? FileManager.default.contentsOfDirectory(atPath: root.path)) ?? []
        let parse: (String) -> [Int]? = { name in
            let parts = name.split(separator: ".").compactMap { Int($0) }
            return parts.count == 3 && name.split(separator: ".").count == 3 ? parts : nil
        }
        return names.compactMap { name in parse(name).map { (name, $0) } }
            .filter { GuardHookPlan.version($0.0, atLeast: [2, 1, 78]) }
            .max { $0.1.lexicographicallyPrecedes($1.1) }?.0
    }
    // Read-only: reports which lifecycle events already call this app's hook command.
    func inspectHooks() -> GuardHookState {
        var state = GuardHookState()
        guard let data = try? Data(contentsOf: claudeSettingsURL) else { return state }
        guard let config = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { state.unreadable = true; return state }
        state.blockedByPolicy = config["disableAllHooks"] as? Bool == true || config["allowManagedHooksOnly"] as? Bool == true
        let command = hookCommand
        for (event, value) in config["hooks"] as? [String: Any] ?? [:] {
            for group in value as? [[String: Any]] ?? [] {
                for hook in group["hooks"] as? [[String: Any]] ?? [] {
                    guard let text = hook["command"] as? String else { continue }
                    if text == command { state.events.append(event) }
                    else if text.contains("/bin/orchestrator' hook --config") { state.stalePaths += 1 }
                }
            }
        }
        state.events = Array(Set(state.events)).sorted()
        return state
    }
    func detectHooks() {
        let state = inspectHooks()
        hooksInstalled = state.events.contains("StopFailure")
        hookStalePaths = state.stalePaths
        if state.blockedByPolicy { hookStatus = "Claude 设置禁止用户 Hooks，需管理员策略允许" }
        else if state.unreadable { hookStatus = "无法解析 ~/.claude/settings.json，不会自动修改" }
        else if hooksInstalled { hookStatus = "已接入 \(state.events.count) 个会话事件（稳定入口，升级无需重接）" + (state.stalePaths > 0 ? "；另有 \(state.stalePaths) 条旧路径，重新接入可清理" : "") }
        else if state.stalePaths > 0 { hookStatus = "现有 \(state.stalePaths) 条 Hook 写死了旧版本的应用路径。改为稳定入口接入一次，以后升级都不用再接。" }
        else { hookStatus = "尚未接入会话事件" }
    }
    // Human-readable plan shown in the consent dialog before anything is written.
    func hookPreview(engineVersion: String) -> String {
        guard let fragment = try? GuardHookPlan.fragment(engineVersion: engineVersion, command: hookCommand) else { return "引擎版本无效，无法生成计划" }
        let existing = Set(inspectHooks().events)
        let events = fragment.keys.sorted().map { existing.contains($0) ? "\($0)（已存在，跳过）" : "+ \($0)" }
        let superseded = supersededHookCount()
        let replaced = superseded > 0 ? "\n- 移除 \(superseded) 条旧版本 LimitBreak 的 Hook 命令（指向同一数据目录的其他路径或版本），避免同一事件被上报多次" : ""
        return "将向 ~/.claude/settings.json 的 hooks 追加：\n" + events.joined(separator: "\n") + replaced + "\n\n命令：\(hookCommand)\n该入口由 LimitBreak 每次启动时指向当前运行的版本；以后升级或移动应用，只需打开一次新版，无需再次接入。\n原文件先备份到应用数据目录，其他配置原样保留。"
    }

    // Invoked only from the native consent dialog; never changes policy/permissions.
    func installHooks(engineVersion: String) {
        do {
            let settings = claudeSettingsURL
            let directory = settings.deletingLastPathComponent()
            if FileManager.default.fileExists(atPath: settings.path), try settings.resourceValues(forKeys: [.isSymbolicLinkKey]).isSymbolicLink == true { throw RuntimeFailure("设置是符号链接，拒绝读取或自动修改") }
            let old = FileManager.default.fileExists(atPath: settings.path) ? try Data(contentsOf: settings) : nil
            var config: [String: Any] = [:]
            if let old {
                guard let decoded = try JSONSerialization.jsonObject(with: old) as? [String: Any] else { throw RuntimeFailure("现有设置不是合法对象，拒绝覆盖") }
                config = decoded
            }
            if config["disableAllHooks"] as? Bool == true || config["allowManagedHooksOnly"] as? Bool == true {
                throw RuntimeFailure("现有策略禁止用户 Hooks，不能自动接入；需要管理员授权。")
            }
            let command = hookCommand
            var hooks = config["hooks"] as? [String: Any] ?? [:]
            if config["hooks"] != nil && config["hooks"] as? [String: Any] == nil { throw RuntimeFailure("已有 Hooks 配置格式无法安全合并") }
            let fragment = try GuardHookPlan.fragment(engineVersion: engineVersion, command: command)
            // Older builds of this app report the same events to the same data directory; keep only this build.
            for (event, value) in hooks {
                guard let groups = value as? [[String: Any]] else { continue }
                let kept: [[String: Any]] = groups.compactMap { group in
                    guard let entries = group["hooks"] as? [[String: Any]] else { return group }
                    let remaining = entries.filter { !isSupersededHook($0["command"] as? String ?? "") }
                    if remaining.count == entries.count { return group }
                    if remaining.isEmpty { return nil }
                    var copy = group
                    copy["hooks"] = remaining
                    return copy
                }
                if kept.isEmpty { hooks.removeValue(forKey: event) } else { hooks[event] = kept }
            }
            for (event, additions) in fragment {
                var groups = hooks[event] as? [[String: Any]] ?? []
                if hooks[event] != nil && hooks[event] as? [[String: Any]] == nil { throw RuntimeFailure("已有会话事件配置格式无法安全合并") }
                for addition in additions {
                    let exists = groups.contains { group in
                        (group["matcher"] as? String ?? "") == (addition["matcher"] as? String ?? "") &&
                        (group["hooks"] as? [[String: Any]] ?? []).contains { $0["command"] as? String == command }
                    }
                    if !exists { groups.append(addition) }
                }
                hooks[event] = groups
            }
            config["hooks"] = hooks
            if let old {
                let backup = dataDirectory.appendingPathComponent("hooks-backup-\(UUID().uuidString).json")
                try GuardPrivateStorage.write(old, to: backup)
            }
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            if FileManager.default.fileExists(atPath: settings.path) {
                guard try Data(contentsOf: settings) == old else { throw RuntimeFailure("设置同时被其他程序修改，已取消写入。") }
            } else if old != nil { throw RuntimeFailure("原设置文件已变化，已取消写入。") }
            try GuardPrivateStorage.write(JSONSerialization.data(withJSONObject: config, options: [.prettyPrinted, .sortedKeys]), to: settings)
            detectHooks()
            notice = "已接入会话事件。已打开的会话需发一条消息后才会出现在列表里。"
            error = ""
        } catch { self.error = (error as? RuntimeFailure)?.message ?? "无法写入会话监控配置。权限或管理员策略可能阻止了接入；没有绕过限制。" }
    }
}
