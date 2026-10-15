import Foundation

// Owns only Process instances created by this app; never adopts or kills a discovered PID.
final class GuardDaemonMonitor: @unchecked Sendable {
    private let lock = NSLock()
    private var generation: UInt64 = 0
    private var process: Process?
    private var errorPipe: Pipe?
    private var errorBytes = 0
    private var categories = Set<String>()
    private var authorizedGeneration: UInt64?
    var identity: UInt64? {
        lock.lock(); defer { lock.unlock() }
        return process?.isRunning == true ? generation : nil
    }
    var authorized: Bool {
        lock.lock(); defer { lock.unlock() }
        return process?.isRunning == true && authorizedGeneration == generation
    }
    func disarm() { lock.lock(); authorizedGeneration = nil; lock.unlock() }
    func arm(_ expected: UInt64) -> Bool {
        lock.lock(); defer { lock.unlock() }
        guard expected == generation, process?.isRunning == true else { return false }
        authorizedGeneration = expected
        return true
    }
    func prepare(_ child: Process, onExit: @escaping (Int32, String) -> Void) {
        let pipe = Pipe()
        child.standardError = pipe
        lock.lock()
        generation &+= 1
        let preparedGeneration = generation
        process = child; errorPipe = pipe; errorBytes = 0; categories = []; authorizedGeneration = nil
        lock.unlock()
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let bytes = handle.availableData
            guard !bytes.isEmpty else { handle.readabilityHandler = nil; return }
            self?.categorize(bytes, generation: preparedGeneration)
        }
        child.terminationHandler = { [weak self] process in
            guard let self else { return }
            pipe.fileHandleForReading.readabilityHandler = nil
            self.lock.lock()
            guard self.generation == preparedGeneration else { self.lock.unlock(); return }
            self.authorizedGeneration = nil
            let names = self.categories.sorted().joined(separator: "、")
            let count = self.errorBytes
            self.lock.unlock()
            let summary = count == 0 ? "后台未提供错误输出" : "后台错误输出 \(count) 字节（仅类别：\(names.isEmpty ? "未分类" : names)，正文未保留）"
            onExit(process.terminationStatus, summary)
        }
    }
    private func categorize(_ data: Data, generation expected: UInt64) {
        let text = String(decoding: data.prefix(4096), as: UTF8.self).lowercased()
        let markers = ["permission": "权限", "address already in use": "端口占用", "unauthorized": "认证", "timeout": "超时", "database": "数据库", "invalid config": "配置", "owner_lock": "后台所有权", "owner lock": "后台所有权"]
        lock.lock(); defer { lock.unlock() }
        guard generation == expected else { return }
        errorBytes = min(1_048_576, errorBytes + data.count)
        for (marker, label) in markers where text.contains(marker) { categories.insert(label) }
    }
    func abortPrepared() {
        lock.lock(); let pipe = errorPipe; process = nil; errorPipe = nil; authorizedGeneration = nil; lock.unlock()
        pipe?.fileHandleForReading.readabilityHandler = nil
    }
}
