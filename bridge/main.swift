import Foundation

// NSWorkspace / AX operations remain on the executable's main thread.
let bridge = NativeBridge()
let arguments = Array(CommandLine.arguments.dropFirst())

func emit<T: Encodable>(_ value: T) {
    do {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        var data = try encoder.encode(value)
        data.append(0x0A)
        try FileHandle.standardOutput.write(contentsOf: data)
    } catch {
        // Do not dump an exception, AX value, request or partially encoded private data.
        exit(74)
    }
}

switch arguments {
case ["--probe"]:
    emit(bridge.probe())
case ["--inspect"]:
    emit(bridge.inspect())
case ["--resume"]:
    do {
        let limit = 131072
        var data = Data()
        while let chunk = try FileHandle.standardInput.read(upToCount: min(16384, limit + 1 - data.count)), !chunk.isEmpty {
            data.append(chunk)
            guard data.count <= limit else {
                throw BridgeFailure("invalid_request", "stdin JSON 超过安全大小限制。")
            }
        }
        let request: ResumeRequest
        do { request = try JSONDecoder().decode(ResumeRequest.self, from: data) }
        catch { throw BridgeFailure("invalid_request", "stdin 必须为完整合法的 binding/message JSON。") }
        emit(bridge.resume(request))
    } catch { emit(bridge.failure(error)) }
default:
    emit(bridge.result("invalid_command", "仅支持 --probe、--inspect 或 --resume（stdin JSON）。"))
}
