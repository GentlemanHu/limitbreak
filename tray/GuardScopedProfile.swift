import Darwin
import Foundation

// Only issued scoped credentials can be exported. Full app tokens never enter this type.
struct GuardScopedProfile: Encodable {
    let daemon_url: String
    let access_token: String
    func validate() throws {
        guard let url = URL(string: daemon_url), url.scheme == "http", url.host == "127.0.0.1",
              let port = url.port, (1...65535).contains(port), url.path.isEmpty, url.user == nil, url.password == nil,
              url.query == nil, url.fragment == nil, access_token.hasPrefix("qso_scope_"),
              GuardMCPContract.digest(String(access_token.dropFirst("qso_scope_".count))) else { throw RuntimeFailure("仅允许导出合法本机 scoped profile，不能导出主应用令牌") }
    }
    func writeNew(to target: URL) throws {
        try validate()
        guard target.isFileURL, !target.lastPathComponent.isEmpty, ![".", ".."].contains(target.lastPathComponent),
              !target.lastPathComponent.unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { throw RuntimeFailure("导出文件路径无效") }
        // Resolve the user-selected parent once; hold its inode while writing the absent file.
        let parent = target.deletingLastPathComponent().resolvingSymlinksInPath()
        let directory = open(parent.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard directory >= 0 else { throw RuntimeFailure("导出目录不可安全打开") }
        defer { close(directory) }
        var directoryInfo = stat()
        guard fstat(directory, &directoryInfo) == 0, directoryInfo.st_uid == getuid(), directoryInfo.st_mode & S_IFMT == S_IFDIR else { throw RuntimeFailure("导出目录须属于当前用户") }
        let fd = openat(directory, target.lastPathComponent, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, mode_t(0o600))
        guard fd >= 0 else { throw RuntimeFailure("导出仅能创建新文件；不覆盖配置、链接或现有profile") }
        var complete = false
        defer {
            close(fd)
            if !complete { _ = unlinkat(directory, target.lastPathComponent, 0) }
        }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_uid == getuid(), info.st_mode & S_IFMT == S_IFREG,
              info.st_mode & 0o777 == 0o600, info.st_nlink == 1 else { throw RuntimeFailure("导出文件必须为自有0600普通文件") }
        let encoder = JSONEncoder(); encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(self)
        var offset = 0
        while offset < data.count {
            let count = data.withUnsafeBytes { Darwin.write(fd, $0.baseAddress?.advanced(by: offset), data.count - offset) }
            if count < 0 && errno == EINTR { continue }
            guard count > 0 else { throw RuntimeFailure("scoped profile写入未完成，令牌不进入日志") }
            offset += count
        }
        guard fsync(fd) == 0, fsync(directory) == 0 else { throw RuntimeFailure("scoped profile同步未确认") }
        complete = true
    }
}
