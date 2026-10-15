import CryptoKit
import Darwin
import Foundation
import Security

// Restrict permissions before writing any bytes; rename only a complete, synced file.
enum GuardPrivateStorage {
    static func read(_ target: URL, limit: Int = 1_048_576) throws -> Data {
        let fd = open(target.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard fd >= 0 else { throw RuntimeFailure("私有配置不存在或路径不安全") }
        defer { close(fd) }
        var info = stat()
        guard fstat(fd, &info) == 0, info.st_uid == getuid(), info.st_mode & S_IFMT == S_IFREG,
              info.st_mode & 0o777 == 0o600, info.st_nlink == 1, info.st_size >= 0, info.st_size <= Int64(limit) else { throw RuntimeFailure("私有配置必须为自有0600普通文件且大小有界") }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 8192)
        while true {
            let count = Darwin.read(fd, &buffer, buffer.count)
            if count < 0 && errno == EINTR { continue }
            guard count >= 0 else { throw RuntimeFailure("私有配置读取失败") }
            if count == 0 { break }
            guard data.count + count <= limit else { throw RuntimeFailure("私有配置超过大小限制") }
            data.append(contentsOf: buffer.prefix(count))
        }
        return data
    }
    static func write(_ data: Data, to target: URL) throws {
        let temporary = target.deletingLastPathComponent().appendingPathComponent(".\(target.lastPathComponent).\(UUID().uuidString)")
        let descriptor = open(temporary.path, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, mode_t(0o600))
        guard descriptor >= 0 else { throw RuntimeFailure("无法创建私有配置文件") }
        let file = FileHandle(fileDescriptor: descriptor, closeOnDealloc: false)
        defer { try? file.close(); try? FileManager.default.removeItem(at: temporary) }
        do {
            try file.write(contentsOf: data)
            try file.synchronize()
            guard rename(temporary.path, target.path) == 0 else { throw RuntimeFailure("无法原子保存私有配置") }
        } catch { throw RuntimeFailure("私有配置未保存成功") }
    }
}

enum GuardSourceCredentials {
    private static let service = "local.quota.session.guard.sources"
    // Changing endpoint, adapter or environment reference never borrows an old secret.
    static func identity(sourceID: String, type: String, url: String, environment: String) -> String {
        let context = [type, url, environment].joined(separator: "\u{0}")
        let digest = SHA256.hash(data: Data(context.utf8)).map { String(format: "%02x", $0) }.joined()
        return sourceID + ":" + digest
    }
    private static func query(_ id: String) -> [String: Any] {
        [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: id]
    }
    static func load(_ id: String) throws -> String? {
        var lookup = query(id)
        lookup[kSecReturnData as String] = true
        lookup[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(lookup as CFDictionary, &result)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let bytes = result as? Data, let secret = String(data: bytes, encoding: .utf8) else { throw RuntimeFailure("无法读取来源钥匙串凭据；没有发出请求") }
        return secret
    }
    static func save(_ secret: String, for id: String) throws {
        let data = Data(secret.utf8)
        let updates = [kSecValueData as String: data]
        let status = SecItemUpdate(query(id) as CFDictionary, updates as CFDictionary)
        if status == errSecItemNotFound {
            var item = query(id)
            item[kSecValueData as String] = data
            item[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
            guard SecItemAdd(item as CFDictionary, nil) == errSecSuccess else { throw RuntimeFailure("来源凭据未能保存至钥匙串") }
        } else if status != errSecSuccess { throw RuntimeFailure("来源凭据未能更新至钥匙串") }
    }
}
