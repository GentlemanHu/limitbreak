import Darwin
import Foundation

final class DesktopIPCServer: @unchecked Sendable {
    private let identity: GuardAppIdentity
    private let queue = DispatchQueue(label: "local.quota.session.guard.ipc")
    private let axQueue = DispatchQueue(label: "local.quota.session.guard.ax", qos: .userInitiated)
    private let state = NSLock()
    private var stopped = false
    private var clients = Set<Int32>()
    private var recentRequests: [String: Date] = [:]
    private var listener: Int32 = -1
    private var source: DispatchSourceRead?
    private var socketInode: ino_t = 0
    private let listen: String
    private let daemonToken: String
    private let daemonAvailable: () -> Bool

    init(identity: GuardAppIdentity, listen: String, daemonToken: String, daemonAvailable: @escaping () -> Bool) {
        self.identity = identity; self.listen = listen; self.daemonToken = daemonToken; self.daemonAvailable = daemonAvailable
    }
    func start() throws {
        try identity.removeStaleSocket()
        listener = socket(AF_UNIX, SOCK_STREAM, 0)
        guard listener >= 0 else { throw RuntimeFailure("无法创建主应用IPC socket") }
        do {
            _ = fcntl(listener, F_SETFD, FD_CLOEXEC)
            _ = fcntl(listener, F_SETFL, O_NONBLOCK)
            var address = sockaddr_un()
            address.sun_family = sa_family_t(AF_UNIX)
            address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
            let path = Array(identity.socketURL.path.utf8) + [0]
            guard path.count <= MemoryLayout.size(ofValue: address.sun_path) else { throw RuntimeFailure("IPC路径超过Unix socket限制") }
            withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: path) }
            let status = withUnsafePointer(to: &address) { pointer in
                pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
            }
            guard status == 0, chmod(identity.socketURL.path, mode_t(0o600)) == 0 else { throw RuntimeFailure("无法绑定安全0600主应用IPC") }
            var info = stat()
            guard lstat(identity.socketURL.path, &info) == 0, info.st_uid == getuid(), info.st_mode & S_IFMT == S_IFSOCK,
                  info.st_mode & 0o777 == 0o600 else { throw RuntimeFailure("主应用IPC文件身份校验失败") }
            socketInode = info.st_ino
            guard Darwin.listen(listener, 4) == 0 else { throw RuntimeFailure("无法监听主应用IPC") }
            let read = DispatchSource.makeReadSource(fileDescriptor: listener, queue: queue)
            read.setEventHandler { [weak self] in self?.acceptClients() }
            let fd = listener
            read.setCancelHandler { close(fd) }
            source = read; read.resume()
        } catch { close(listener); listener = -1; throw error }
    }
    func stop() {
        state.lock(); stopped = true; let active = clients; state.unlock()
        // Shutdown wakes readers and invalidates every in-flight AX gate; workers own close().
        for fd in active { _ = Darwin.shutdown(fd, SHUT_RDWR) }
        source?.cancel(); source = nil
        var info = stat()
        if socketInode != 0, lstat(identity.socketURL.path, &info) == 0, info.st_uid == getuid(),
           info.st_mode & S_IFMT == S_IFSOCK, info.st_ino == socketInode { _ = unlink(identity.socketURL.path) }
    }
    private func acceptClients() {
        for _ in 0..<8 {
            let fd = accept(listener, nil, nil)
            if fd < 0 { return }
            _ = fcntl(fd, F_SETFD, FD_CLOEXEC)
            _ = fcntl(fd, F_SETFL, O_NONBLOCK)
            var noSignal: Int32 = 1
            _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSignal, socklen_t(MemoryLayout<Int32>.size))
            var uid: uid_t = 0, gid: gid_t = 0
            guard getpeereid(fd, &uid, &gid) == 0, uid == getuid() else { close(fd); continue }
            state.lock()
            let allowed = !stopped && clients.count < 4
            if allowed { clients.insert(fd) }
            state.unlock()
            guard allowed else { close(fd); continue }
            DispatchQueue.global(qos: .utility).async { [self] in receive(fd) }
        }
    }
    private func alive(_ fd: Int32, deadline: Date) -> Bool {
        state.lock(); let running = !stopped && clients.contains(fd); state.unlock()
        guard running, deadline > Date(), daemonAvailable() else { return false }
        var byte: UInt8 = 0
        let count = recv(fd, &byte, 1, MSG_PEEK | MSG_DONTWAIT)
        // EOF, extra frames, or socket errors cancel; idle open connection is valid.
        return count < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)
    }
    private func finish(_ fd: Int32) {
        state.lock(); clients.remove(fd); state.unlock()
        close(fd)
    }
    private func receive(_ fd: Int32) {
        var data = Data()
        let until = Date().addingTimeInterval(2)
        do {
            while Date() < until {
                state.lock(); let active = !stopped; state.unlock()
                guard active else { throw DesktopIPCRequest.invalid() }
                var item = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
                let status = poll(&item, 1, 50)
                if status == 0 { continue }
                guard status > 0, item.revents & Int16(POLLERR | POLLNVAL) == 0 else { throw DesktopIPCRequest.invalid() }
                var buffer = [UInt8](repeating: 0, count: 8192)
                let count = recv(fd, &buffer, buffer.count, 0)
                guard count > 0 else { throw DesktopIPCRequest.invalid() }
                data.append(contentsOf: buffer.prefix(count))
                guard data.count <= 131073 else { throw DesktopIPCRequest.invalid() }
                if let newline = data.firstIndex(of: 0x0A) {
                    guard newline == data.count - 1 else { throw DesktopIPCRequest.invalid() }
                    data.removeLast()
                    let request = try DesktopIPCRequest.decode(data, expectedToken: identity.token)
                    data.removeAll(keepingCapacity: false)
                    guard remember(request) else { throw DesktopIPCRequest.invalid() }
                    axQueue.async { [self] in execute(request, fd: fd) }
                    return
                }
            }
            throw DesktopIPCRequest.invalid()
        } catch {
            // Unauthenticated requests get no response or reflection of their request_id.
            finish(fd)
        }
    }
    private func remember(_ request: DesktopIPCRequest) -> Bool {
        state.lock(); defer { state.unlock() }
        recentRequests = recentRequests.filter { $0.value > Date() }
        guard !stopped, recentRequests.count < 256, recentRequests[request.request_id] == nil else { return false }
        recentRequests[request.request_id] = Date().addingTimeInterval(60)
        return true
    }
    private func execute(_ request: DesktopIPCRequest, fd: Int32) {
        defer { finish(fd) }
        let cancelled = { [self] in !alive(fd, deadline: request.deadlineDate) }
        let bridge = NativeBridge(deadline: request.deadlineDate, userIdleSeconds: request.user_idle_seconds ?? 3,
                                  quietSeconds: request.quiet_seconds ?? 0, restoreFocus: request.restore_focus ?? false, cancelled: cancelled,
                                  eligibility: { [self] in
                                      guard daemonAvailable() else { throw BridgeFailure("execution_ineligible", "自有后台已退出；禁止发送。") }
                                      try GuardExecutionGate(request: request, listen: listen, token: daemonToken).check(cancelled: cancelled)
                                  })
        var response: DesktopIPCResponse
        guard !cancelled() else { return }
        switch request.operation {
        case "probe": response = DesktopIPCResponse(request_id: request.request_id, result: bridge.probe())
        case "inspect":
            let inspection = bridge.inspect()
            response = DesktopIPCResponse(request_id: request.request_id, result: inspection.result, inspection: inspection)
        case "validate_target":
            guard let binding = request.binding else { return }
            response = DesktopIPCResponse(request_id: request.request_id, result: bridge.validateTarget(binding))
        case "locate":
            guard let binding = request.binding else { return }
            response = DesktopIPCResponse(request_id: request.request_id, result: bridge.locate(binding))
        case "wake":
            guard let binding = request.binding else { return }
            response = DesktopIPCResponse(request_id: request.request_id, result: bridge.wake(binding))
        case "resume":
            guard let binding = request.binding, let message = request.message else { return }
            response = DesktopIPCResponse(request_id: request.request_id, result: bridge.resume(ResumeRequest(binding: binding, message: message)))
        default: return
        }
        guard !cancelled(), var bytes = try? JSONEncoder().encode(response), bytes.count + 1 <= 65536 else { return }
        bytes.append(0x0A)
        let end = min(request.deadlineDate, Date().addingTimeInterval(1))
        var offset = 0
        while offset < bytes.count && Date() < end {
            guard !cancelled() else { return }
            let sent = bytes.withUnsafeBytes { send(fd, $0.baseAddress?.advanced(by: offset), bytes.count - offset, 0) }
            if sent > 0 { offset += sent }
            else if sent < 0 && (errno == EAGAIN || errno == EWOULDBLOCK) {
                var item = pollfd(fd: fd, events: Int16(POLLOUT), revents: 0); _ = poll(&item, 1, 20)
            } else { return }
        }
    }
}
