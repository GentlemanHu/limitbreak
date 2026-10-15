import Foundation

enum GuardHookPlan {
    static func version(_ text: String, atLeast wanted: [Int]) -> Bool {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        let value = trimmed.hasPrefix("v") ? String(trimmed.dropFirst()) : trimmed
        let parts = value.components(separatedBy: ".")
        guard parts.count == 3, parts.allSatisfy({ !$0.isEmpty && $0.utf8.allSatisfy { (48...57).contains($0) } }),
              let major = Int(parts[0]), let minor = Int(parts[1]), let patch = Int(parts[2]), wanted.count == 3 else { return false }
        let actual = [major, minor, patch]
        for i in 0..<3 { if actual[i] != wanted[i] { return actual[i] > wanted[i] } }
        return true
    }
    static func fragment(engineVersion: String, command: String) throws -> [String: [[String: Any]]] {
        guard version(engineVersion, atLeast: [2, 1, 78]) else { throw RuntimeFailure("请填写已确认的内嵌 Code 引擎版本；StopFailure 至少需2.1.78，不用独立CLI版本代替") }
        var events = ["SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd"]
        if version(engineVersion, atLeast: [2, 1, 251]) { events.append("PostModelSwitch") }
        var result: [String: [[String: Any]]] = [:]
        for event in events { result[event] = [["hooks": [["type": "command", "command": command, "timeout": 2]]]] }
        if version(engineVersion, atLeast: [2, 1, 234]) {
            result["Notification"] = [["matcher": "quota_auto_resume_fired|quota_auto_resume_stale|quota_auto_resume_disabled", "hooks": [["type": "command", "command": command, "timeout": 2]]]]
        }
        return result
    }
}
