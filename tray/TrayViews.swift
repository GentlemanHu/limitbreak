import AppKit
import SwiftUI

// The status-bar popover: what will happen automatically, every account's
// 5-hour and weekly usage, the sessions waiting, and the last few events.
struct TrayPanel: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var refreshing = false
    private static let accountLimit = 6
    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            hostingRow
            callouts
            stats
            accountsSection
            waitingSection
            activitySection
            Divider()
            footer
        }.padding(16).frame(width: 410)
    }

    private var header: some View {
        HStack(spacing: 11) {
            Image(systemName: headerIcon.0).font(.system(size: 26)).foregroundStyle(headerIcon.1)
            VStack(alignment: .leading, spacing: 3) {
                Text("LimitBreak").font(.headline)
                Text(statusLine).font(.caption).foregroundStyle(.secondary).lineLimit(2)
            }
            Spacer()
            EmergencyPauseButton(runtime: runtime, compact: true).buttonStyle(.bordered).controlSize(.small)
        }
    }
    private var headerIcon: (String, Color) {
        if !runtime.localServiceFresh || !runtime.proxyConnected { return ("bolt.horizontal.circle", .secondary) }
        if runtime.paused { return ("pause.circle.fill", .red) }
        if runtime.attentionCount > 0 { return ("exclamationmark.circle.fill", .orange) }
        if runtime.autoSendArmed && runtime.hostedReadyCount > 0 { return ("bolt.horizontal.circle.fill", .blue) }
        return ("eye.circle.fill", .teal)
    }
    private var statusLine: String {
        if [.starting, .checking].contains(runtime.sourcePhase) { return "正在读取额度来源…" }
        if !runtime.localServiceFresh { return "等待本地服务" }
        if !runtime.proxyConnected { return "额度来源未连接" }
        return runtime.hostingSummary
    }

    private var hostingRow: some View {
        HStack(spacing: 10) {
            Image(systemName: "wand.and.stars").foregroundStyle(runtime.defaultHosted ? Color.accentColor : .secondary)
            VStack(alignment: .leading, spacing: 1) {
                Text("默认托管").font(.callout)
                Text("只影响「跟随全局」的 \(runtime.followingSessionCount) 个会话；单独设置的不受影响").font(.caption2).foregroundStyle(.secondary).lineLimit(2)
            }
            Spacer()
            DefaultHostingToggle(runtime: runtime, showsLabel: false).controlSize(.small)
        }.padding(10).background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 9))
    }

    @ViewBuilder private var callouts: some View {
        if !runtime.error.isEmpty {
            HStack(alignment: .top, spacing: 6) {
                Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                Text(runtime.error).font(.caption).lineLimit(3)
                Spacer()
                Button { runtime.error = "" } label: { Image(systemName: "xmark") }.buttonStyle(.plain).help("关闭提示")
            }
        }
        if runtime.localServiceFresh && !runtime.paused && !runtime.automationConsented && runtime.hostedSessionCount > 0 {
            HStack(spacing: 8) {
                Image(systemName: "hand.raised.fill").foregroundStyle(.orange)
                Text("\(runtime.hostedSessionCount) 个会话设为托管，但尚未允许自动继续").font(.caption)
                Spacer()
                ConsentButton(runtime: runtime).controlSize(.small)
            }
        }
        if runtime.attentionCount > 0 || setupIncomplete {
            Button { runtime.openWorkbench(runtime.attentionCount > 0 ? .sessions : .overview) } label: {
                Label(runtime.attentionCount > 0 ? "\(runtime.attentionCount) 个会话需要处理" : "还有设置步骤未完成", systemImage: "exclamationmark.triangle.fill")
                    .font(.callout).foregroundStyle(.orange).frame(maxWidth: .infinity, alignment: .leading)
            }.buttonStyle(.plain)
        }
    }
    private var setupIncomplete: Bool { !runtime.proxyConnected || !runtime.accessibilityGranted || !runtime.hooksInstalled }

    private var stats: some View {
        HStack(spacing: 8) {
            stat("托管", runtime.hostedSessionCount, icon: "bolt.fill", color: runtime.autoSendArmed ? .blue : .secondary, target: .sessions)
            stat("监控", runtime.monitoredSessionCount, icon: "eye.fill", color: .teal, target: .sessions)
            stat("等待恢复", runtime.waitingCount, icon: "hourglass", color: runtime.waitingCount > 0 ? .indigo : .secondary, target: .recovery)
            stat("需处理", runtime.attentionCount, icon: "exclamationmark.triangle.fill", color: runtime.attentionCount > 0 ? .orange : .secondary, target: .recovery)
        }
    }
    private func stat(_ title: String, _ value: Int, icon: String, color: Color, target: WorkbenchSection) -> some View {
        Button { runtime.openWorkbench(target) } label: {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 4) { Image(systemName: icon).font(.caption2).foregroundStyle(color); Text("\(value)").font(.title3.weight(.semibold)).monospacedDigit() }
                Text(title).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
            }.padding(8).frame(maxWidth: .infinity, alignment: .leading).background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 8)).contentShape(Rectangle())
        }.buttonStyle(.plain)
    }

    @ViewBuilder private var accountsSection: some View {
        // Limited and unconfirmed accounts first: they are why someone opens the menu.
        let accounts = (runtime.snapshot?.accounts ?? []).filter { $0.disabled != true }.sorted { a, b in
            let ra = urgency(a), rb = urgency(b)
            return ra != rb ? ra < rb : SessionTally(runtime.sessions(boundTo: a.id)).total > SessionTally(runtime.sessions(boundTo: b.id)).total
        }
        if !accounts.isEmpty {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Text("额度账号").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    Spacer()
                    if let next = runtime.nextCheck, runtime.waitingCount > 0, next > runtime.now {
                        Label("下次复查 " + trayDuration(next.timeIntervalSince(runtime.now)), systemImage: "clock").font(.caption2.monospacedDigit()).foregroundStyle(.blue)
                    }
                }
                ForEach(Array(accounts.prefix(Self.accountLimit))) { account in
                    Button { runtime.openWorkbench(.resources) } label: { accountRow(account) }.buttonStyle(.plain)
                }
                if accounts.count > Self.accountLimit {
                    Button("还有 \(accounts.count - Self.accountLimit) 个账号 →") { runtime.openWorkbench(.resources) }.buttonStyle(.link).font(.caption)
                }
            }
        }
    }
    private func urgency(_ account: TrayAccount) -> Int {
        guard runtime.isFresh(account) else { return 1 }
        return account.state == "limited" ? 0 : account.state == "allowed" ? 2 : 1
    }
    private func accountRow(_ account: TrayAccount) -> some View {
        let layout = AccountQuotaLayout(account)
        let badge = runtime.accountBadge(account)
        let tally = SessionTally(runtime.sessions(boundTo: account.id))
        return HStack(alignment: .center, spacing: 9) {
            ProviderIcon(provider: account.provider, size: 24)
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 5) {
                    Text(account.email.isEmpty ? account.id : account.email).font(.caption.weight(.medium)).lineLimit(1).truncationMode(.middle)
                    Spacer(minLength: 2)
                    if tally.total > 0 { Label("\(tally.total)", systemImage: "bubble.left").font(.caption2).foregroundStyle(.secondary).help(tally.detail) }
                    QuotaStateBadge(text: badge.0, color: badge.1)
                }
                HStack(spacing: 10) {
                    QuotaBar(window: layout.worstShort, placeholder: "5 小时", now: runtime.now, compact: true)
                    QuotaBar(window: layout.worstWeekly, placeholder: "每周", now: runtime.now, compact: true)
                }
            }
        }.contentShape(Rectangle())
    }

    @ViewBuilder private var waitingSection: some View {
        let waiting = runtime.liveSessions.filter { ["NEEDS_ATTENTION", "COOLDOWN", "READY_TO_RESUME", "VERIFYING", "DISPATCHING"].contains($0.state) }
        if !waiting.isEmpty {
            VStack(alignment: .leading, spacing: 6) {
                HStack {
                    Text("等待中的会话").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    Spacer()
                    Button { runtime.showRecoveryWindow?() } label: { Label("恢复任务面板", systemImage: "timer") }
                        .buttonStyle(.borderless).font(.caption).help("在独立窗口中查看所有恢复任务的实时倒计时")
                }
                ForEach(Array(waiting.prefix(4))) { session in
                    Button { runtime.openWorkbench(.recovery) } label: { sessionRow(session) }.buttonStyle(.plain)
                }
                if waiting.count > 4 { Text("另有 \(waiting.count - 4) 个").font(.caption2).foregroundStyle(.secondary) }
            }
        }
    }
    private func sessionRow(_ session: TraySession) -> some View {
        HStack(spacing: 8) {
            Circle().fill(session.state == "NEEDS_ATTENTION" ? Color.orange : Color.blue).frame(width: 6, height: 6)
            Text(session.displayTitle).font(.callout).lineLimit(1)
            Text(session.host?.mode == "hosted" || session.host?.mode == "legacy" ? "托管" : "监控").font(.caption2).foregroundStyle(.secondary)
                .padding(.horizontal, 5).background(.quaternary, in: Capsule())
            Spacer()
            let clock = runtime.clock(for: session)
            if session.state == "COOLDOWN", let at = clock.expected, at > runtime.now {
                Label(trayDuration(at.timeIntervalSince(runtime.now)), systemImage: "clock.badge.checkmark").font(.caption.monospacedDigit()).foregroundStyle(.indigo)
                    .help("预计 " + at.formatted(date: .omitted, time: .shortened) + " 恢复后自动继续，以复查为准")
            } else if clock.block != .none {
                Label(clock.block == .concurrency ? "等并发名额" : clock.block == .desktop ? "等桌面就绪" : "宽限期", systemImage: "pause.circle")
                    .font(.caption).foregroundStyle(.orange).help(clock.blockText)
            } else if let at = clock.nextCheck, at > runtime.now {
                Label(trayDuration(at.timeIntervalSince(runtime.now)), systemImage: "arrow.triangle.2.circlepath").font(.caption.monospacedDigit()).foregroundStyle(.secondary).help("下次查询额度")
            } else if session.state == "VERIFYING", let channel = DeliveryChannel(job: runtime.lastDelivery(for: session)) {
                Label(channel.title(terminal: session.origin?.app == "terminal"), systemImage: channel.icon).font(.caption).foregroundStyle(channel.color)
                    .help("已发送，等待会话确认：" + channel.detail(terminal: session.origin?.app == "terminal"))
            } else { Text(trayState(session.state)).font(.caption).foregroundStyle(.secondary) }
        }.contentShape(Rectangle())
    }

    @ViewBuilder private var activitySection: some View {
        let recent = Array((runtime.snapshot?.audit ?? []).suffix(3).reversed())
        if !recent.isEmpty {
            VStack(alignment: .leading, spacing: 5) {
                Text("最近活动").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                ForEach(recent) { item in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Text(trayAgo(trayDate(item.time), now: runtime.now)).font(.caption2.monospacedDigit()).foregroundStyle(.secondary).frame(width: 58, alignment: .leading)
                        Text(item.localized).font(.caption).lineLimit(1).truncationMode(.tail).help(item.localized)
                    }
                }
            }
        }
    }

    private var footer: some View {
        HStack {
            Button { runtime.openWorkbench() } label: { Label("工作台", systemImage: "macwindow") }
            Button { runtime.showRecoveryWindow?() } label: { Label("恢复任务", systemImage: "timer") }.help("独立窗口：所有恢复任务的实时倒计时")
            Button { refreshing = true; Task { await runtime.refreshQuota(); refreshing = false } } label: { Label(refreshing ? "查询中" : "刷新", systemImage: "arrow.clockwise") }
                .disabled(refreshing || !runtime.daemonRunning).help("立即重新查询额度（同一账号 60 秒内只查一次，上游限流时自动退避）")
            Button { runtime.toggleAwake() } label: { Label("防睡眠", systemImage: runtime.awake ? "cup.and.saucer.fill" : "cup.and.saucer") }
                .tint(runtime.awake ? .blue : nil).help(runtime.awake ? "已阻止系统闲置睡眠；点按恢复" : "阻止系统闲置睡眠，让等待中的会话能按时继续")
            Spacer()
            Button { NSApplication.shared.terminate(nil) } label: { Image(systemName: "power") }.help("退出 LimitBreak")
        }.buttonStyle(.bordered).controlSize(.small)
    }
}
