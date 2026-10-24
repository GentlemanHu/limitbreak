import SwiftUI

// Live view of the latest test send: each step comes from the job's state and
// reason in the 3-second snapshot, so what is shown is what the daemon recorded.
struct SessionTestProgress: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    let session: TraySession
    var body: some View {
        if let job = runtime.latestTest(for: session) {
            GroupBox {
                VStack(alignment: .leading, spacing: 10) {
                    HStack {
                        Text(L("测试发送进度", "Test Send Progress")).font(.headline)
                        Spacer()
                        if let start = trayDate(job.created_at) {
                            Text(start.formatted(Date.FormatStyle(date: .omitted, time: .standard, locale: lang.locale)) + L(" 开始", " started")).font(.caption.monospacedDigit()).foregroundStyle(.secondary)
                        }
                    }
                    let stage = TestStage(job: job)
                    ForEach(0..<TestStage.titles.count, id: \.self) { index in
                        step(index, stage: stage, job: job)
                    }
                    if let outcome = stage.outcome {
                        Text(outcome.text).font(.callout).foregroundStyle(outcome.ok ? Color.green : Color.orange)
                            .fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
            }
        }
    }
    @ViewBuilder private func step(_ index: Int, stage: TestStage, job: TrayJob) -> some View {
        let status = stage.status(of: index)
        HStack(alignment: .top, spacing: 10) {
            Group {
                switch status {
                case .done: Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                case .active: ProgressView().controlSize(.small)
                case .waiting: Image(systemName: "pause.circle.fill").foregroundStyle(.orange)
                case .failed: Image(systemName: "xmark.circle.fill").foregroundStyle(.red)
                case .pending: Image(systemName: "circle").foregroundStyle(.tertiary)
                }
            }.frame(width: 18)
            VStack(alignment: .leading, spacing: 2) {
                Text(TestStage.titles[index]).font(.callout.weight(status == .pending ? .regular : .semibold))
                    .foregroundStyle(status == .pending ? Color.secondary : Color.primary)
                if let detail = stage.detail(of: index, job: job, now: runtime.now) {
                    Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            }
        }
    }
}

private struct TestStage {
    enum Status { case done, active, waiting, failed, pending }
    static var titles: [String] {
        [L("排队", "Queueing"),
         L("打开会话、核验目标并发送", "Open Session, Verify & Send"),
         L("等待会话回执", "Awaiting Turn Receipt"),
         L("完成", "Complete")]
    }
    let current: Int
    let failed: Bool
    let waiting: Bool
    let reason: String
    let state: String
    init(job: TrayJob) {
        state = job.state
        let raw = job.reason ?? ""
        reason = LanguageManager.shared.explain(reason: raw, reasonText: job.reason_text)
        waiting = state == "READY_TO_RESUME" && raw.hasPrefix("waiting for desktop")
        switch state {
        case "READY_TO_RESUME", "COOLDOWN": current = 0; failed = false
        case "DISPATCHING": current = 1; failed = false
        case "VERIFYING": current = 2; failed = false
        case "COMPLETED": current = 4; failed = false
        default:
            current = raw.hasPrefix("dispatch confirmation timed out") ? 2 : (raw.hasPrefix("未发送") || state == "CANCELLED" ? 1 : 2)
            failed = true
        }
    }
    func status(of index: Int) -> Status {
        if index < current { return .done }
        if index > current { return .pending }
        if failed { return .failed }
        return waiting ? .waiting : .active
    }
    func detail(of index: Int, job: TrayJob, now: Date) -> String? {
        guard index == current else { return nil }
        switch (index, failed) {
        case (0, _):
            if waiting {
                let tries = job.attempts.map { L("（已尝试 \($0)/10 次）", " (attempted \($0)/10)") } ?? ""
                return reason + tries
            }
            if let at = trayDate(job.not_before), at > now { return localizedDuration(at.timeIntervalSince(now)) + L(" 后开始", " until start") }
            return L("即将开始；接下来请不要操作键盘和鼠标", "Starting soon; please release keyboard and mouse")
        case (1, false): return L("正在切到 Claude 并发送；请勿操作键盘和鼠标", "Switching to Claude and sending; please do not touch keyboard or mouse")
        case (2, false):
            let since = trayDate(job.updated_at).map { L("，已等待 \(Int(max(0, now.timeIntervalSince($0)))) 秒", ", waiting for \(Int(max(0, now.timeIntervalSince($0))))s") } ?? ""
            return L("已按下发送，等待 Claude 上报该会话收到了消息（最多约 45 秒）", "Send pressed; awaiting confirmation hook from Claude (up to 45s)") + since
        default: return nil
        }
    }
    var outcome: (ok: Bool, text: String)? {
        if state == "COMPLETED" { return (true, L("✓ 会话已确认收到测试文案，目标会话正确。", "✓ Session acknowledged receipt; target session verified.")) }
        guard failed else { return nil }
        if reason.hasPrefix("未发送") || reason.hasPrefix("Not sent") {
            return (false, reason + "\n" + L("没有写入或发送任何内容，可以修正后再次测试。", "Nothing was written or sent; you may adjust settings and retry."))
        }
        if state == "CANCELLED" { return (false, L("已停止：", "Stopped: ") + reason) }
        return (false, reason + "\n" + L("结果不确定：请到 Claude 中查看该会话；为避免重复，不会自动重发。", "Outcome uncertain: please check session in Claude; will not resend to avoid duplicate."))
    }
}
