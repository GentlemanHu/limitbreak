import SwiftUI

struct WorkbenchOverview: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    let navigate: (WorkbenchSection) -> Void
    @State private var connecting = false
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                VStack(alignment: .leading, spacing: 6) {
                    Text(headline).font(.largeTitle.bold())
                    Text(L("额度用完时记住每个会话；额度恢复后，按各自的模型和账号池，把各自的继续文案发回原来的窗口。",
                           "Remembers each session when quota runs out. Once quota recovers, delivers each session's continue message back to its window or terminal.")).foregroundStyle(.secondary)
                }
                if setupDone < 4 {
                    GroupBox {
                        VStack(alignment: .leading, spacing: 0) {
                            HStack {
                                Text(L("首次设置", "Initial Setup")).font(.headline)
                                Spacer()
                                Text(L("\(setupDone)/4 完成", "\(setupDone)/4 Complete")).font(.caption).foregroundStyle(.secondary)
                            }.padding(.bottom, 8)
                            step(1, done: runtime.proxyConnected, busy: sourceBusy, title: L("连接额度来源", "Connect Quota Source"), detail: sourceDetail) {
                                HStack {
                                    if runtime.sourcePhase == .failed { Button(L("重试", "Retry")) { Task { await runtime.refreshQuota() } } }
                                    Button(runtime.configured ? L("连接设置", "Settings") : L("连接 CLIProxyAPI", "Connect CLIProxyAPI")) { connecting = true }.buttonStyle(.borderedProminent)
                                }
                            }
                            Divider()
                            step(2, done: runtime.accessibilityGranted, title: L("允许辅助功能", "Grant Accessibility"), detail: L("向 Claude 桌面输入框写入继续文案需要此权限", "Required to type continue prompt into Claude input box")) { AccessibilityActions(runtime: runtime) }
                            Divider()
                            step(3, done: runtime.hooksInstalled, title: L("接入会话事件", "Connect Session Events"), detail: hookPurposeText + "\n" + runtime.hookStatus) { HookInstallButton(runtime: runtime) }
                            Divider()
                            step(4, done: runtime.automationConsented, title: L("允许自动继续", "Authorize Auto-Continue"), detail: L("确认后，智能托管的会话在额度恢复时会自动发送继续文案", "Once confirmed, hosted sessions will automatically continue when quota recovers")) { ConsentButton(runtime: runtime) }
                        }.padding(8)
                    }
                }
                HStack(spacing: 14) {
                    stat(L("可用额度组", "Available Groups"), value: runtime.availableGroupCount, icon: "chart.bar", target: .resources)
                    stat(L("托管就绪会话", "Hosted Sessions"), value: runtime.hostedReadyCount, icon: "checkmark.seal", target: .sessions)
                    stat(L("等待额度恢复", "Waiting Sessions"), value: runtime.waitingCount, icon: "clock", target: .recovery)
                }
                let failedSources = (runtime.snapshot?.sources ?? []).filter { $0.state == "error" }
                if !failedSources.isEmpty {
                    callout(L("\(failedSources.count) 个额度来源查询失败，相关会话暂不自动继续。", "\(failedSources.count) quota source(s) failed; affected sessions will not auto-continue."), action: L("查看来源", "View Sources")) { navigate(.sources) }
                }
                GroupBox(L("需要你看一眼的会话", "Sessions Needing Attention")) {
                    VStack(alignment: .leading, spacing: 10) {
                        let items = attention
                        if items.isEmpty { Text(L("没有等待或需要处理的会话。", "No waiting or attention-needed sessions.")).foregroundStyle(.secondary) }
                        ForEach(Array(items.prefix(6))) { session in
                            Button { navigate(.sessions) } label: {
                                HStack(alignment: .top) {
                                    VStack(alignment: .leading, spacing: 3) {
                                        Text(session.displayTitle).lineLimit(1)
                                        Text(session.host?.problem ?? session.explanation).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                                    }
                                    Spacer()
                                    if let job = runtime.job(for: session), let at = trayDate(job.not_before), at > runtime.now {
                                        Text(localizedDuration(at.timeIntervalSince(runtime.now))).font(.caption.monospacedDigit())
                                    } else { Text(localizedState(session.state)).font(.caption).foregroundStyle(.secondary) }
                                }.contentShape(Rectangle())
                            }.buttonStyle(.plain)
                        }
                        Button(L("打开会话托管", "Open Hosted Sessions")) { navigate(.sessions) }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(8)
                }
            }.padding(28).frame(maxWidth: 980, alignment: .leading)
        }.sheet(isPresented: $connecting) { ConnectionSheet(runtime: runtime, close: { connecting = false }) }
    }
    private var hookPurposeText: String {
        L("Claude 只有通过会话事件才会告诉 LimitBreak「这个会话撞上额度上限了」以及「继续文案已真正送达」。没有它，会话列表照常显示、可手动测试发送，但不会自动发现限额、自动排队续写。",
          "Claude reports limit-reached and delivery confirmation events to LimitBreak via session hooks. Without it, sessions are listed and testable, but cannot auto-detect rate limits or auto-queue.")
    }
    private var sourceBusy: Bool { [.starting, .checking].contains(runtime.sourcePhase) }
    private var sourceDetail: String {
        switch runtime.sourcePhase {
        case .unconfigured: return L("连接本机 CLIProxyAPI，读取 Claude、Codex、Antigravity 的短期/每周额度", "Connect local CLIProxyAPI to read 5-hour and weekly quotas")
        case .starting: return L("正在启动后台服务…", "Starting background daemon…")
        case .checking: return L("后台已就绪，正在向 CLIProxyAPI 读取各账号额度（已等待 \(runtime.secondsSinceLaunch) 秒，通常几秒内完成）", "Daemon ready; reading account quotas from CLIProxyAPI (waiting \(runtime.secondsSinceLaunch)s)")
        case .connected: return L("CLIProxyAPI 已连接", "CLIProxyAPI Connected")
        case .failed: return runtime.sourceProblem
        }
    }
    private var setupDone: Int { [runtime.proxyConnected, runtime.accessibilityGranted, runtime.hooksInstalled, runtime.automationConsented].filter { $0 }.count }
    private var headline: String {
        if setupDone < 4 { return L("还差 \(4 - setupDone) 步即可自动托管", "\(4 - setupDone) step(s) left to enable auto-hosting") }
        if runtime.paused { return L("已紧急暂停，只监控", "Emergency paused · Monitor only") }
        if runtime.waitingCount > 0 { return L("\(runtime.waitingCount) 个会话在等待额度恢复", "\(runtime.waitingCount) session(s) waiting for quota recovery") }
        return runtime.hostedReadyCount > 0 ? L("一切正常，\(runtime.hostedReadyCount) 个会话托管中", "All systems normal · \(runtime.hostedReadyCount) session(s) hosted") : L("一切正常，正在监控", "All systems normal · Monitoring")
    }
    private var attention: [TraySession] {
        let active: Set<String> = ["NEEDS_ATTENTION", "COOLDOWN", "READY_TO_RESUME", "VERIFYING", "DISPATCHING"]
        let listed = runtime.sessions.filter { session -> Bool in
            if session.isArchived && runtime.job(for: session) == nil { return false }
            if active.contains(session.state) { return true }
            return session.host?.mode == "hosted" && session.host?.ready == false
        }
        // Sessions needing attention first; the sort is stable otherwise.
        return listed.filter { $0.state == "NEEDS_ATTENTION" } + listed.filter { $0.state != "NEEDS_ATTENTION" }
    }
    private func step<Actions: View>(_ index: Int, done: Bool, busy: Bool = false, title: String, detail: String, @ViewBuilder actions: () -> Actions) -> some View {
        HStack(alignment: .top, spacing: 14) {
            Group {
                if busy && !done { ProgressView().controlSize(.small) }
                else { Image(systemName: done ? "checkmark.circle.fill" : "\(index).circle").font(.title2).foregroundStyle(done ? Color.green : Color.secondary) }
            }.frame(width: 26, height: 26)
            VStack(alignment: .leading, spacing: 6) {
                Text(title).font(.headline)
                Text(detail).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if !done && !busy { actions() }
            }
            Spacer()
        }.padding(.vertical, 11)
    }
    private func callout(_ text: String, action: String, perform: @escaping () -> Void) -> some View {
        HStack { Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange); Text(text); Spacer(); Button(action, action: perform) }
            .padding(12).background(.orange.opacity(0.08), in: RoundedRectangle(cornerRadius: 10))
    }
    private func stat(_ title: String, value: Int, icon: String, target: WorkbenchSection) -> some View {
        Button { navigate(target) } label: {
            VStack(alignment: .leading, spacing: 12) { Label(title, systemImage: icon).font(.callout).foregroundStyle(.secondary); Text("\(value)").font(.system(size: 32, weight: .semibold, design: .rounded)).monospacedDigit() }
                .frame(maxWidth: .infinity, alignment: .leading).padding(18).background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 12))
        }.buttonStyle(.plain).accessibilityLabel("\(title)：\(value)，打开\(target.title)")
    }
}

// Request, open System Settings, and relaunch: a stale code identity in TCC only clears after re-adding the app.
struct AccessibilityActions: View {
    @ObservedObject var runtime: TrayRuntime
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Button(L("请求授权", "Request Authorization")) { runtime.requestAccessibility() }.buttonStyle(.borderedProminent)
                Button(L("打开系统设置", "Open System Settings")) { runtime.openAccessibilitySettings() }
                Button(L("重启 LimitBreak", "Relaunch LimitBreak")) { runtime.relaunch() }
            }
            if runtime.accessibilityObservation.requestedAt != nil {
                Text(L("系统设置里已打开却仍显示未授权？那是旧版本留下的记录：在列表中选中「LimitBreak」点「−」删除，再点「+」重新添加 /Applications/LimitBreak.app，然后点「重启 LimitBreak」。",
                       "Toggled on in System Settings but still shows unauthorized? That is a stale TCC entry: select 'LimitBreak' in the list, click '−' to remove, click '+' to re-add /Applications/LimitBreak.app, then click 'Relaunch LimitBreak'."))
                    .font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
            }
            if let checked = runtime.accessibilityObservation.checkedAt {
                Text(L("每 2 秒自动复查 · 上次 ", "Checked every 2s · Last checked ") + checked.formatted(date: .omitted, time: .standard)).font(.caption2).foregroundStyle(.secondary)
            }
        }
    }
}

struct HookInstallButton: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var version = ""
    @State private var confirming = false
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                TextField(L("内嵌 Code 引擎版本", "Embedded Code Engine Version"), text: $version).textFieldStyle(.roundedBorder).frame(width: 170)
                Button(runtime.hooksInstalled ? L("重新接入", "Reinstall Hooks") : runtime.hookStalePaths > 0 ? L("查看改动并改为稳定入口", "Review & Update Entry") : L("查看改动并接入", "Review & Install Hooks")) { confirming = true }
                    .disabled(!GuardHookPlan.version(version, atLeast: [2, 1, 78]))
            }
            Text(GuardHookPlan.version(version, atLeast: [2, 1, 78])
                 ? L("已自动识别 Claude 桌面内嵌引擎版本；可手动修改", "Auto-detected embedded Claude Code engine version; editable")
                 : L("未识别到 2.1.78 以上的内嵌引擎；请先更新 Claude 桌面", "Engine version 2.1.78+ not detected; please update Claude desktop")).font(.caption2).foregroundStyle(.secondary)
        }
        .onAppear { if version.isEmpty { version = runtime.detectedEngineVersion() ?? "" } }
        .alert(L("接入 Claude 会话事件", "Connect Claude Session Events"), isPresented: $confirming) {
            Button(L("取消", "Cancel"), role: .cancel) {}
            Button(L("确认写入", "Confirm Write")) { runtime.installHooks(engineVersion: version) }
        } message: {
            Text(runtime.hookPreview(engineVersion: version) + "\n\n" + L("只记录会话 ID、模型、项目、来源窗口和限额事件；不保存提示词或回复正文。", "Records session ID, model, directory, window source, and rate-limit events only; never saves prompts or replies."))
        }
    }
}

struct ConsentButton: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var confirming = false
    @State private var busy = false
    var body: some View {
        Button(busy ? L("保存中…", "Saving…") : L("允许自动继续…", "Authorize Auto-Continue…")) { confirming = true }.buttonStyle(.borderedProminent).disabled(busy || !runtime.localServiceFresh)
            .alert(L("允许 LimitBreak 自动发送继续文案？", "Allow LimitBreak to auto-send continue messages?"), isPresented: $confirming) {
                Button(L("取消", "Cancel"), role: .cancel) {}
                Button(L("允许自动继续", "Authorize")) { busy = true; Task { _ = await runtime.grantAutomationConsent(); busy = false } }
            } message: { Text(consentExplanation) }
    }
}
