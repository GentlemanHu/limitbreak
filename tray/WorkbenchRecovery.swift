import AppKit
import SwiftUI

// Every recovery in one place: a live clock, what is waiting and how long until
// the nearest continue, then one card per task. Shown as a workbench page and,
// on request, in its own always-on-top window.
struct WorkbenchRecovery: View {
    @ObservedObject var runtime: TrayRuntime
    var standalone = false
    @State private var filter = RecoveryFilter.active
    var body: some View {
        let tasks = runtime.recoveryTasks(filter)
        VStack(spacing: 0) {
            RecoveryHeader(runtime: runtime, standalone: standalone)
            HStack {
                Picker("", selection: $filter) {
                    ForEach(RecoveryFilter.allCases) { Text($0.title).tag($0) }
                }.pickerStyle(.segmented).frame(maxWidth: 460)
                Spacer()
                Text(L("每秒刷新 · 额度以复查为准", "Refreshed every second · Subject to live quota check")).font(.caption).foregroundStyle(.secondary)
            }.padding(.horizontal, 18).padding(.vertical, 10)
            Divider()
            if tasks.isEmpty {
                // Fill the remaining space so the header stays pinned to the top.
                WorkbenchEmpty(title: emptyTitle, detail: emptyDetail, icon: "timer")
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollView {
                    LazyVStack(spacing: 12) {
                        ForEach(tasks) { RecoveryCard(runtime: runtime, task: $0) }
                    }.padding(16)
                }.frame(maxWidth: .infinity, maxHeight: .infinity)
            }
        }.frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }
    private var emptyTitle: String {
        switch filter {
        case .active: return L("没有正在等待的恢复", "No Active Recovery Tasks")
        case .attention: return L("没有需要处理的恢复", "No Tasks Needing Attention")
        case .finished: return L("近 24 小时没有已结束的恢复", "No Finished Tasks in Last 24h")
        case .all: return L("还没有恢复任务", "No Recovery Tasks Yet")
        }
    }
    private var emptyDetail: String {
        L("会话撞上限额后，这里会出现一条恢复任务：从撞限额起计时，倒数到额度重置，复查确认后自动发送继续，并等会话回执确认。",
          "When a session hits rate limits, a recovery task appears here: counts down to quota reset, verifies recovery, sends continue prompt, and awaits turn receipt.")
    }
}

// The top strip: wall clock, counts and the nearest continue.
private struct RecoveryHeader: View {
    @ObservedObject var runtime: TrayRuntime
    let standalone: Bool
    @AppStorage(RecoveryWindow.floatingKey) private var floating = true
    var body: some View {
        let summary = runtime.recoverySummary
        let locale = LanguageManager.shared.locale
        HStack(alignment: .center, spacing: 14) {
            VStack(alignment: .leading, spacing: 2) {
                Text(runtime.now.formatted(Date.FormatStyle(date: .omitted, time: .standard, locale: locale))).font(.system(size: 30, weight: .semibold, design: .rounded).monospacedDigit())
                Text(runtime.now.formatted(Date.FormatStyle().weekday(.wide).month().day().locale(locale))).font(.caption).foregroundStyle(.secondary)
            }.frame(minWidth: 150, alignment: .leading)
            Divider().frame(height: 44)
            stat(L("等待中", "Waiting"), summary.waiting, "hourglass", .indigo)
            stat(L("发送中", "Sending"), summary.sending, "paperplane", .blue)
            stat(L("需处理", "Attention"), summary.attention, "exclamationmark.triangle", summary.attention > 0 ? .red : .secondary)
            stat(L("24h 已继续", "24h Done"), summary.finished, "checkmark.seal", .green)
            Divider().frame(height: 44)
            VStack(alignment: .leading, spacing: 2) {
                Text(L("最近一次自动继续", "Nearest Auto-Continue")).font(.caption).foregroundStyle(.secondary)
                if let nearest = summary.nearest {
                    Text(localizedDuration(nearest.at.timeIntervalSince(runtime.now))).font(.title2.weight(.semibold).monospacedDigit()).foregroundStyle(.blue)
                    Text("「\(nearest.task.title)」· " + recoveryTime(nearest.at, now: runtime.now)).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                } else {
                    Text(summary.waiting > 0 ? L("以复查为准", "Subject to check") : "—").font(.title3.weight(.semibold))
                }
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 6) {
                Label(runtime.hostingSummary, systemImage: runtime.paused ? "pause.octagon" : "bolt.horizontal.circle").font(.caption).foregroundStyle(runtime.paused ? .red : .secondary).lineLimit(2)
                if standalone {
                    HStack(spacing: 6) {
                        Toggle(isOn: $floating) { Label(L("置顶", "Pin"), systemImage: floating ? "pin.fill" : "pin") }
                            .toggleStyle(.button).controlSize(.small).help(L("窗口保持在其他应用之上", "Keep window on top of other applications"))
                        Button { runtime.openWorkbench(.recovery) } label: { Label(L("工作台", "Workbench"), systemImage: "macwindow") }.controlSize(.small)
                    }
                } else {
                    Button { runtime.showRecoveryWindow?() } label: { Label(L("独立窗口", "Pop-out Window"), systemImage: "rectangle.portrait.on.rectangle.portrait") }
                        .controlSize(.small).help(L("在单独的置顶窗口里显示所有恢复任务", "Open all recovery tasks in a separate floating window"))
                }
            }.frame(maxWidth: 260, alignment: .trailing)
        }.padding(.horizontal, 18).padding(.vertical, 12).background(.quaternary.opacity(0.25))
    }
    private func stat(_ title: String, _ value: Int, _ icon: String, _ color: Color) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Label(title, systemImage: icon).font(.caption).foregroundStyle(.secondary)
            Text("\(value)").font(.title2.weight(.semibold).monospacedDigit()).foregroundStyle(value > 0 ? color : .secondary)
        }.frame(minWidth: 70, alignment: .leading)
    }
}

// The recovery page in a window of its own, floating above other apps by default
// so the countdowns stay visible while working in Claude.
@MainActor
final class RecoveryWindow {
    private let window: NSWindow
    private var closing: NSObjectProtocol?
    static let floatingKey = "QSORecoveryFloating"
    init(runtime: TrayRuntime, closed: @escaping @MainActor () -> Void) {
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 860, height: 680), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        window.title = L("LimitBreak · 恢复任务", "LimitBreak · Recovery Tasks")
        window.minSize = NSSize(width: 640, height: 420)
        window.isReleasedWhenClosed = false
        window.setFrameAutosaveName("LimitBreakRecovery")
        window.contentView = NSHostingView(rootView: RecoveryWindowContent(runtime: runtime, window: window))
        let floating = UserDefaults.standard.object(forKey: Self.floatingKey) as? Bool ?? true
        window.level = floating ? .floating : .normal
        window.collectionBehavior = [.fullScreenAuxiliary]
        window.center()
        closing = NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: window, queue: .main) { _ in
            MainActor.assumeIsolated { closed() }
        }
    }
    func show() {
        window.makeKeyAndOrderFront(nil)
        NSApplication.shared.activate(ignoringOtherApps: true)
    }
}

private struct RecoveryWindowContent: View {
    @ObservedObject var runtime: TrayRuntime
    let window: NSWindow
    @AppStorage(RecoveryWindow.floatingKey) private var floating = true
    var body: some View {
        WorkbenchRecovery(runtime: runtime, standalone: true)
            .onChange(of: floating) { _, on in window.level = on ? .floating : .normal }
            .frame(minWidth: 640, minHeight: 420)
    }
}
