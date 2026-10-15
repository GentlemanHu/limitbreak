import Darwin
import Foundation

struct RuntimeFailure: Error { let message: String; init(_ message: String) { self.message = message } }

@main
struct PrivateStorageTest {
    static func main() throws {
        guard let temporaryRoot = ProcessInfo.processInfo.environment["TMPDIR"], !temporaryRoot.isEmpty else {
            throw RuntimeFailure("私有存储测试需要显式 TMPDIR 临时目录。")
        }
        let directory = URL(fileURLWithPath: temporaryRoot, isDirectory: true).appendingPathComponent("qso-private-storage-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        defer { try? FileManager.default.removeItem(at: directory) }
        let target = directory.appendingPathComponent("config.json")
        let first = Data("isolated-private-storage-contract".utf8)
        try GuardPrivateStorage.write(first, to: target)
        let bytes = try Data(contentsOf: target)
        precondition(bytes == first)
        let firstMode = try FileManager.default.attributesOfItem(atPath: target.path)[.posixPermissions] as? NSNumber
        precondition(firstMode?.intValue == 0o600)
        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: target.path)
        let next = Data("replacement-contract".utf8)
        try GuardPrivateStorage.write(next, to: target)
        let nextMode = try FileManager.default.attributesOfItem(atPath: target.path)[.posixPermissions] as? NSNumber
        precondition(nextMode?.intValue == 0o600)
        let replacement = try Data(contentsOf: target)
        precondition(replacement == next)
        let entries = try FileManager.default.contentsOfDirectory(atPath: directory.path)
        precondition(entries == ["config.json"])
        let slot = GuardSourceCredentials.identity(sourceID: "source", type: "readonly-http", url: "https://quota.invalid/a", environment: "QSO_SOURCE_KEY")
        precondition(slot == GuardSourceCredentials.identity(sourceID: "source", type: "readonly-http", url: "https://quota.invalid/a", environment: "QSO_SOURCE_KEY"))
        precondition(slot != GuardSourceCredentials.identity(sourceID: "source", type: "readonly-http", url: "https://quota.invalid/b", environment: "QSO_SOURCE_KEY"))
        precondition(slot != GuardSourceCredentials.identity(sourceID: "source", type: "cliproxyapi", url: "https://quota.invalid/a", environment: "QSO_SOURCE_KEY"))
        precondition(slot != GuardSourceCredentials.identity(sourceID: "source", type: "readonly-http", url: "https://quota.invalid/a", environment: "QSO_OTHER_KEY"))
        precondition(slot != GuardSourceCredentials.identity(sourceID: "other", type: "readonly-http", url: "https://quota.invalid/a", environment: "QSO_SOURCE_KEY"))
        print("私有文件原子写入、权限、凭据作用域与清理验证通过；未读取生产配置或操作钥匙串。")
    }
}
