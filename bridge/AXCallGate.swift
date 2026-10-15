import Foundation

// A per-request worker-thread gate. Every AX attribute/action query checks cancellation.
final class AXCallGate: NSObject {
    private let check: () -> Bool
    init(check: @escaping () -> Bool) { self.check = check }
    private static let key = "local.quota.session.guard.ax-call-gate"
    static var permitted: Bool {
        (Thread.current.threadDictionary[key] as? AXCallGate)?.check() ?? true
    }
    static func withGate<T>(_ check: @escaping () -> Bool, _ body: () -> T) -> T {
        let previous = Thread.current.threadDictionary[key]
        Thread.current.threadDictionary[key] = AXCallGate(check: check)
        defer {
            if let previous { Thread.current.threadDictionary[key] = previous }
            else { Thread.current.threadDictionary.removeObject(forKey: key) }
        }
        return body()
    }
    static func require() throws {
        guard permitted else { throw BridgeFailure("cancelled", "请求已取消、连接关闭或期限已到；停止 AX 操作。") }
    }
}
