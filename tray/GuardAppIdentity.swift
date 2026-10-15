import Darwin
import Foundation
import Security

// Holds the stable lock until this app exits; duplicates never touch token/socket/config.
final class GuardAppIdentity {
    let directory: URL
    let socketURL: URL
    let tokenURL: URL
    private(set) var token = ""
    private var directoryFD: Int32 = -1
    private var lockFD: Int32 = -1

    init(directory: URL) throws {
        self.directory = directory
        socketURL = directory.appendingPathComponent("desktop.sock")
        tokenURL = directory.appendingPathComponent("desktop-token")
        try Self.requireNoSymlinks(directory.deletingLastPathComponent())
        if mkdir(directory.path, mode_t(0o700)) != 0 && errno != EEXIST { throw RuntimeFailure("无法创建应用私有数据目录") }
        directoryFD = open(directory.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard directoryFD >= 0 else { throw RuntimeFailure("应用数据目录不是安全目录") }
        do {
            var info = stat()
            guard fstat(directoryFD, &info) == 0, info.st_uid == getuid(), info.st_mode & S_IFMT == S_IFDIR,
                  info.st_mode & 0o777 == 0o700 else { throw RuntimeFailure("应用数据目录必须由当前用户所有且为0700；未自动更改不安全目录") }
            lockFD = openat(directoryFD, "app.lock", O_RDWR | O_CREAT | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
            guard lockFD >= 0 else { throw RuntimeFailure("无法创建稳定应用锁") }
            try Self.requirePrivateFile(lockFD)
            guard flock(lockFD, LOCK_EX | LOCK_NB) == 0 else { throw RuntimeFailure("另一个 LimitBreak 实例正在使用此数据目录；此实例不会旋转token或启动后台") }
            token = try loadOrCreateToken()
        } catch {
            if lockFD >= 0 { close(lockFD); lockFD = -1 }
            close(directoryFD); directoryFD = -1
            throw error
        }
    }
    deinit {
        if lockFD >= 0 { flock(lockFD, LOCK_UN); close(lockFD) }
        if directoryFD >= 0 { close(directoryFD) }
    }
    static func requireNoSymlinks(_ url: URL) throws {
        var path = URL(fileURLWithPath: "/", isDirectory: true)
        for component in url.standardizedFileURL.pathComponents.dropFirst() {
            path.appendPathComponent(component)
            var info = stat()
            guard lstat(path.path, &info) == 0, info.st_mode & S_IFMT == S_IFDIR else { throw RuntimeFailure("私有路径含符号链接或非目录组件") }
        }
    }
    static func requirePrivateFile(_ fd: Int32) throws {
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_uid == getuid(), info.st_mode & S_IFMT == S_IFREG,
              info.st_mode & 0o777 == 0o600, info.st_nlink == 1 else { throw RuntimeFailure("私有文件必须为当前用户0600普通文件，且没有硬链接") }
    }
    private func loadOrCreateToken() throws -> String {
        var fd = openat(directoryFD, "desktop-token", O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        if fd < 0 && errno == ENOENT {
            var random = [UInt8](repeating: 0, count: 32)
            guard SecRandomCopyBytes(kSecRandomDefault, random.count, &random) == errSecSuccess else { throw RuntimeFailure("无法获得安全随机IPC token") }
            let text = random.map { String(format: "%02x", $0) }.joined()
            fd = openat(directoryFD, "desktop-token", O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
            guard fd >= 0 else { throw RuntimeFailure("无法独占创建IPC token") }
            defer { close(fd) }
            try Self.requirePrivateFile(fd)
            let data = Data(text.utf8)
            let count = data.withUnsafeBytes { Darwin.write(fd, $0.baseAddress, $0.count) }
            guard count == data.count, fsync(fd) == 0, fsync(directoryFD) == 0 else { throw RuntimeFailure("IPC token保存未完成") }
            return text
        }
        guard fd >= 0 else { throw RuntimeFailure("IPC token路径不安全或不可读") }
        defer { close(fd) }
        try Self.requirePrivateFile(fd)
        var bytes = [UInt8](repeating: 0, count: 65)
        let count = Darwin.read(fd, &bytes, bytes.count)
        guard count == 64, let text = String(bytes: bytes.prefix(64), encoding: .utf8), DesktopIPCRequest.isHex(text, count: 64) else {
            throw RuntimeFailure("现有IPC token格式无效；不会自动旋转破坏运行者身份")
        }
        return text
    }
    func removeStaleSocket() throws {
        var info = stat()
        if lstat(socketURL.path, &info) != 0 {
            guard errno == ENOENT else { throw RuntimeFailure("无法核验IPC socket路径") }
            return
        }
        guard info.st_uid == getuid(), info.st_mode & S_IFMT == S_IFSOCK, info.st_mode & 0o777 == 0o600 else {
            throw RuntimeFailure("IPC socket旧路径不是自有0600 socket，拒绝覆盖")
        }
        guard unlinkat(directoryFD, "desktop.sock", 0) == 0 else { throw RuntimeFailure("无法移除自有旧IPC socket") }
    }
}
