import Foundation

// Called only on the IPC AX worker, never the UI thread. No retries or body logging.
final class GuardExecutionGate: NSObject, URLSessionDataDelegate, @unchecked Sendable {
    private let lock = NSLock()
    private let done = DispatchSemaphore(value: 0)
    private var bytes = Data()
    private var validResponse = false
    private var successful = false
    private let request: DesktopIPCRequest
    private let listen: String
    private let token: String
    init(request: DesktopIPCRequest, listen: String, token: String) {
        self.request = request; self.listen = listen; self.token = token
    }
    func check(cancelled: () -> Bool) throws {
        guard !Thread.isMainThread, !cancelled(), request.deadlineDate > Date(),
              let binding = request.binding, let generation = request.generation, let lease = request.lease_id,
              let hash = request.message_hash, let url = URL(string: "http://\(listen)/api/desktop/execution"),
              url.host == "127.0.0.1", url.user == nil, url.password == nil, url.query == nil, url.fragment == nil else { throw denied() }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.connectionProxyDictionary = [:]
        configuration.urlCache = nil
        configuration.httpCookieStorage = nil
        configuration.timeoutIntervalForRequest = 3
        configuration.timeoutIntervalForResource = 3
        let session = URLSession(configuration: configuration, delegate: self, delegateQueue: nil)
        defer { session.invalidateAndCancel() }
        var http = URLRequest(url: url)
        http.httpMethod = "POST"
        http.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        http.setValue("application/json", forHTTPHeaderField: "Content-Type")
        http.httpBody = try JSONSerialization.data(withJSONObject: ["session_id": binding.sessionID, "generation": generation, "lease_id": lease, "message_hash": hash])
        session.dataTask(with: http).resume()
        let end = min(request.deadlineDate, Date().addingTimeInterval(3))
        while done.wait(timeout: .now() + 0.05) != .success {
            if cancelled() || Date() >= end { throw denied() }
        }
        lock.lock(); let ok = successful; lock.unlock()
        guard ok, !cancelled(), request.deadlineDate > Date() else { throw denied() }
    }
    private func denied() -> BridgeFailure { BridgeFailure("execution_ineligible", "本机守护器未确认当前generation与lease有效；拒绝写入或发送。") }
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive response: URLResponse,
                    completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
        lock.lock()
        validResponse = (response as? HTTPURLResponse).map { (200..<300).contains($0.statusCode) } == true && response.expectedContentLength <= 4096
        let valid = validResponse
        lock.unlock()
        completionHandler(valid ? .allow : .cancel)
    }
    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        lock.lock()
        let oversized = bytes.count + data.count > 4096
        if !oversized { bytes.append(data) }
        lock.unlock()
        if oversized { dataTask.cancel() }
    }
    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        lock.lock()
        if error == nil, validResponse, let object = (try? JSONSerialization.jsonObject(with: bytes)) as? [String: Any],
           let ok = object["ok"] as? NSNumber, CFGetTypeID(ok) == CFBooleanGetTypeID(), ok.boolValue,
           let eligible = object["eligible"] as? NSNumber, CFGetTypeID(eligible) == CFBooleanGetTypeID(), eligible.boolValue {
            successful = true
        }
        bytes.removeAll(keepingCapacity: false)
        lock.unlock()
        done.signal()
    }
}
