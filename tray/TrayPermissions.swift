import AppKit
import ApplicationServices
import Foundation

extension TrayRuntime {
    func checkAccessibility() {
        // AXIsProcessTrusted is read-only; no TCC database or user configuration access.
        let granted = AXIsProcessTrusted()
        accessibilityObservation.state = granted ? .granted : .denied
        accessibilityObservation.checkedAt = Date()
    }
    func requestAccessibility() {
        // Only called by the user's explicit native button, never via daemon or MCP.
        checkAccessibility()
        guard accessibilityObservation.state != .granted else { return }
        accessibilityObservation.requestedAt = Date()
        let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        let granted = AXIsProcessTrustedWithOptions(options)
        accessibilityObservation.state = granted ? .granted : .denied
        accessibilityObservation.checkedAt = Date()
    }
    func openAccessibilitySettings() {
        accessibilityObservation.requestedAt = Date()
        if let url = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility") { NSWorkspace.shared.open(url) }
    }
    // macOS caches a denied trust decision per process; a fresh process reads the switch the user just turned on.
    func relaunch() {
        let bundle = Bundle.main.bundlePath
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sh")
        process.arguments = ["-c", "while kill -0 \"$1\" 2>/dev/null; do sleep 0.2; done; /usr/bin/open \"$0\"", bundle, String(ProcessInfo.processInfo.processIdentifier)]
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
            NSApplication.shared.terminate(nil)
        } catch { self.error = "无法自动重启，请手动退出后重新打开 LimitBreak" }
    }
}
