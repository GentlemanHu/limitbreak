import AppKit
import SwiftUI

enum WorkbenchSection: String, CaseIterable, Identifiable {
    case overview, recovery, sessions, resources, hosting, activity, sources, diagnostics, mcp, settings
    var id: String { rawValue }
    var title: String {
        switch self {
        case .overview: return "概览"; case .recovery: return "恢复任务"; case .sessions: return "会话托管"; case .resources: return "额度资源"; case .hosting: return "托管规则"
        case .activity: return "活动记录"; case .sources: return "来源与适配"; case .diagnostics: return "诊断"; case .mcp: return "MCP 授权"; case .settings: return "设置"
        }
    }
    var icon: String {
        switch self {
        case .overview: return "square.grid.2x2"; case .recovery: return "timer"; case .sessions: return "bubble.left.and.bubble.right"; case .resources: return "chart.bar"; case .hosting: return "wand.and.stars"
        case .activity: return "list.bullet.rectangle"; case .sources: return "puzzlepiece.extension"; case .diagnostics: return "stethoscope"; case .mcp: return "key.horizontal"; case .settings: return "gearshape"
        }
    }
}

@MainActor
final class WorkbenchNavigation: ObservableObject {
    @Published var section: WorkbenchSection? = .overview
}

@MainActor
final class WorkbenchWindow {
    private let window: NSWindow
    private let navigation = WorkbenchNavigation()
    private var closing: NSObjectProtocol?
    init(runtime: TrayRuntime, closed: @escaping @MainActor () -> Void) {
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1120, height: 760), styleMask: [.titled, .closable, .resizable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "LimitBreak"
        window.minSize = NSSize(width: 840, height: 600)
        window.isReleasedWhenClosed = false
        window.setFrameAutosaveName("QuotaGuardWorkbench")
        window.contentView = NSHostingView(rootView: WorkbenchView(runtime: runtime, navigation: navigation))
        window.center()
        closing = NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: window, queue: .main) { _ in
            MainActor.assumeIsolated { closed() }
        }
    }
    func show(_ section: WorkbenchSection = .overview) {
        navigation.section = section
        window.makeKeyAndOrderFront(nil)
        NSApplication.shared.activate(ignoringOtherApps: true)
    }
}

struct WorkbenchView: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject var navigation: WorkbenchNavigation
    @State private var connecting = false
    @State private var refreshing = false
    var body: some View {
        NavigationSplitView {
            VStack(alignment: .leading, spacing: 0) {
                Label("LimitBreak", systemImage: "bolt.horizontal.circle").font(.title3.bold()).padding(20)
                List(WorkbenchSection.allCases, selection: $navigation.section) { section in
                    Label(section.title, systemImage: section.icon).tag(section)
                }.listStyle(.sidebar)
                Divider()
                VStack(alignment: .leading, spacing: 7) {
                    Button { connecting = true } label: {
                        switch runtime.sourcePhase {
                        case .connected: Label("额度来源已连接", systemImage: "checkmark.circle")
                        case .starting, .checking: HStack(spacing: 6) { ProgressView().controlSize(.mini); Text("正在读取额度来源…") }
                        case .failed: Label("额度来源读取失败", systemImage: "exclamationmark.triangle").foregroundStyle(.orange)
                        case .unconfigured: Label("额度来源未连接", systemImage: "link.badge.plus")
                        }
                    }.buttonStyle(.plain).help("打开连接设置")
                    Label(runtime.hostingSummary, systemImage: runtime.paused ? "pause.octagon" : runtime.autoSendArmed && runtime.hostedReadyCount > 0 ? "bolt.horizontal.circle" : "eye")
                        .lineLimit(2)
                }.font(.caption).foregroundStyle(.secondary).padding(16)
            }.navigationSplitViewColumnWidth(min: 190, ideal: 210, max: 260)
        } detail: {
            VStack(spacing: 0) {
                HostingBanner(runtime: runtime)
                if !runtime.notice.isEmpty && runtime.error.isEmpty {
                    HStack(spacing: 10) {
                        Image(systemName: "checkmark.circle").foregroundStyle(.green)
                        Text(runtime.notice).font(.callout)
                        Spacer()
                        Button { runtime.notice = "" } label: { Image(systemName: "xmark") }.buttonStyle(.plain).help("关闭提示")
                    }.padding(12).background(.green.opacity(0.07))
                }
                if !runtime.error.isEmpty {
                    HStack(alignment: .top, spacing: 10) {
                        Image(systemName: "exclamationmark.triangle").foregroundStyle(.orange)
                        Text(runtime.error).font(.callout).textSelection(.enabled)
                        Spacer()
                        Button { runtime.error = "" } label: { Image(systemName: "xmark") }.buttonStyle(.plain).help("关闭提示")
                    }.padding(14).background(.orange.opacity(0.08))
                }
                detail.frame(maxWidth: .infinity, maxHeight: .infinity)
            }.navigationTitle((navigation.section ?? .overview).title)
                .toolbar {
                    ToolbarItemGroup {
                        Button { connecting = true } label: { Label(connectionTitle, systemImage: connectionIcon) }
                            .labelStyle(.titleAndIcon).help("查看和修改 CLIProxyAPI 连接（额度来源）")
                        Button { refreshing = true; Task { await runtime.refreshQuota(); refreshing = false } } label: { Label(refreshing ? "读取中" : "刷新额度", systemImage: "arrow.clockwise") }
                            .labelStyle(.titleAndIcon).disabled(refreshing || !runtime.daemonRunning).help("立即向额度来源重新读取各账号额度")
                        DefaultHostingToggle(runtime: runtime)
                        EmergencyPauseButton(runtime: runtime).labelStyle(.titleAndIcon)
                    }
                }
        }.sheet(isPresented: $connecting) { ConnectionSheet(runtime: runtime, close: { connecting = false }) }
    }
    private var connectionTitle: String {
        switch runtime.sourcePhase {
        case .connected: return "已连接"
        case .starting, .checking: return "连接中"
        case .failed: return "连接异常"
        case .unconfigured: return "连接额度来源"
        }
    }
    private var connectionIcon: String {
        switch runtime.sourcePhase {
        case .connected: return "link"
        case .starting, .checking: return "link"
        case .failed: return "exclamationmark.triangle"
        case .unconfigured: return "link.badge.plus"
        }
    }
    @ViewBuilder private var detail: some View {
        switch navigation.section ?? .overview {
        case .overview: WorkbenchOverview(runtime: runtime, navigate: { navigation.section = $0 })
        case .recovery: WorkbenchRecovery(runtime: runtime)
        case .sessions: WorkbenchSessions(runtime: runtime)
        case .resources: WorkbenchResources(runtime: runtime)
        case .hosting: WorkbenchHosting(runtime: runtime)
        case .activity: WorkbenchActivity(runtime: runtime)
        case .sources: WorkbenchSources(runtime: runtime)
        case .diagnostics: WorkbenchDiagnostics(runtime: runtime)
        case .mcp: WorkbenchMCP(runtime: runtime)
        case .settings: WorkbenchSettings(runtime: runtime)
        }
    }
}
