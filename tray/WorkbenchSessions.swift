import SwiftUI

private enum SessionFilter: String, CaseIterable, Identifiable {
    case all, attention, waiting, hosted, monitor
    var id: String { rawValue }
    var title: String {
        switch self {
        case .all: return L("全部", "All")
        case .attention: return L("需处理", "Attention")
        case .waiting: return L("等待恢复", "Waiting")
        case .hosted: return L("智能托管", "Hosted")
        case .monitor: return L("仅监控/关闭", "Monitor / Off")
        }
    }
    func matches(_ session: TraySession) -> Bool {
        switch self {
        case .all: return true
        case .attention: return session.state == "NEEDS_ATTENTION" || (session.host?.mode == "hosted" && session.host?.ready == false)
        case .waiting: return ["COOLDOWN", "READY_TO_RESUME", "VERIFYING", "DISPATCHING"].contains(session.state)
        case .hosted: return session.host?.mode == "hosted"
        case .monitor: return ["monitor", "off", "legacy"].contains(session.host?.mode ?? "")
        }
    }
}

func trayRelative(_ date: Date, now: Date) -> String {
    let formatter = RelativeDateTimeFormatter()
    formatter.locale = LanguageManager.shared.locale
    formatter.unitsStyle = .short
    return formatter.localizedString(for: min(date, now), relativeTo: now)
}

struct WorkbenchSessions: View {
    @ObservedObject var runtime: TrayRuntime
    @ObservedObject private var lang = LanguageManager.shared
    @State private var search = ""
    @State private var filter = SessionFilter.all
    @State private var showArchived = false
    @State private var selected: String?
    private var archivedCount: Int { runtime.sessions.filter(\.isArchived).count }
    private var filtered: [TraySession] {
        runtime.index.sessionsByActivity.filter { (showArchived || !$0.isArchived) && filter.matches($0) && matchesSearch($0) }
    }
    private func matchesSearch(_ s: TraySession) -> Bool {
        search.isEmpty || "\(s.title) \(s.id) \(s.cwd) \(s.model) \(s.origin?.appTitle ?? "") \(s.origin?.host_session_id ?? "")".localizedCaseInsensitiveContains(search)
    }
    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                TextField(L("搜索标题、项目、模型或会话 ID", "Search title, project, model, or session ID"), text: $search).textFieldStyle(.roundedBorder)
                Picker("", selection: $filter) { ForEach(SessionFilter.allCases) { Text($0.title).tag($0) } }.pickerStyle(.segmented).frame(width: 360)
                Toggle(L("显示已归档", "Show Archived"), isOn: $showArchived).toggleStyle(.checkbox)
            }.padding([.horizontal, .top], 16)
            Text(summary).font(.caption).foregroundStyle(.secondary).frame(maxWidth: .infinity, alignment: .leading)
                .padding([.horizontal, .bottom], 16).padding(.top, 6)
            Divider()
            HSplitView {
                List(filtered, selection: $selected) { session in SessionRow(runtime: runtime, session: session).tag(session.id) }
                    .frame(minWidth: 260, idealWidth: 310, maxWidth: 380)
                Group {
                    if let session = runtime.sessions.first(where: { $0.id == selected }) {
                        SessionDetail(runtime: runtime, session: session).id(session.id)
                    } else {
                        WorkbenchEmpty(title: runtime.sessions.isEmpty ? L("还没有会话", "No Sessions Yet") : L("选择一个会话", "Select a Session"),
                                       detail: runtime.sessions.isEmpty ? L("LimitBreak 每 10 秒读取一次 Claude 桌面的会话列表；打开 Claude 桌面后，这里会列出它侧栏中的 Code 会话。", "LimitBreak reads Claude desktop sessions every 10s. Open Claude desktop to populate Code sessions here.") : L("查看它的额度池、发送目标，设置托管方式和继续文案，或先测试发送一次确认目标正确。", "View quota pool, send target, set hosting mode and continue prompt, or test send to verify target."),
                                       icon: "bubble.left.and.bubble.right")
                    }
                }.frame(minWidth: 440, maxWidth: .infinity, maxHeight: .infinity)
            }
        }.onAppear { if !takeFocus() && selected == nil { selected = filtered.first?.id } }
            .onChange(of: runtime.focusSession) { _, _ in _ = takeFocus() }
    }
    private func takeFocus() -> Bool {
        guard let id = runtime.focusSession else { return false }
        runtime.focusSession = nil
        if runtime.sessions.first(where: { $0.id == id })?.isArchived == true { showArchived = true }
        filter = .all; search = ""; selected = id
        return true
    }
    private var summary: String {
        var text = L("与 Claude 桌面侧栏同步（每 10 秒），按最近活动排序。显示 \(filtered.count) / \(runtime.sessions.count) 个",
                     "Synced with Claude Desktop sidebar (every 10s), sorted by recent activity. Showing \(filtered.count) of \(runtime.sessions.count)")
        if !showArchived && archivedCount > 0 {
            text += L("，另有 \(archivedCount) 个已归档或已删除的会话已隐藏", ", \(archivedCount) archived/deleted sessions hidden")
        }
        return text + L("。终端里的 claude 会话在接入会话事件后出现。", ". Terminal claude sessions appear once hooks are connected.")
    }
}

private struct SessionRow: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: icon).foregroundStyle(.secondary).frame(width: 18).padding(.top, 2)
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(session.displayTitle).font(.headline).lineLimit(1)
                    if session.isArchived { tag(L("已归档", "Archived"), .gray) }
                }
                Text([session.projectName, session.model.isEmpty ? L("模型未知", "Model unknown") : session.model].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(.secondary).lineLimit(1)
                HStack(spacing: 6) {
                    status
                    if let at = runtime.lastActive(session) { Text(trayRelative(at, now: runtime.now)).font(.caption2).foregroundStyle(.secondary) }
                }
            }
        }.padding(.vertical, 4).opacity(session.isArchived ? 0.6 : 1)
    }
    @ViewBuilder private var status: some View {
        let clock = runtime.clock(for: session)
        if let job = runtime.job(for: session), job.kind == "test", let at = trayDate(job.not_before), at > runtime.now {
            tag(L("测试 ", "Test ") + localizedDuration(at.timeIntervalSince(runtime.now)), .blue)
        } else if session.state == "COOLDOWN", let at = clock.expected, at > runtime.now {
            tag(L("预计 ", "Est. ") + localizedDuration(at.timeIntervalSince(runtime.now)), .indigo)
                .help(L("预计 " + at.formatted(date: .abbreviated, time: .shortened) + " 恢复，以复查为准",
                        "Estimated recovery at " + at.formatted(date: .abbreviated, time: .shortened) + ", subject to check"))
            if let next = clock.nextCheck, next > runtime.now {
                tag(L("复查 ", "Check ") + localizedDuration(next.timeIntervalSince(runtime.now)), .blue)
            }
        } else if let job = runtime.job(for: session), let at = trayDate(job.not_before), at > runtime.now {
            tag(L("复查 ", "Check ") + localizedDuration(at.timeIntervalSince(runtime.now)), .blue)
        } else if session.state == "NEEDS_ATTENTION" {
            tag(L("需处理", "Attention"), .orange)
        } else if ["READY_TO_RESUME", "DISPATCHING", "VERIFYING", "COOLDOWN"].contains(session.state) {
            tag(localizedState(session.state), .blue)
            if session.state == "VERIFYING", let channel = DeliveryChannel(job: runtime.lastDelivery(for: session)) {
                tag(channel.title(terminal: session.origin?.app == "terminal"), channel.color).help(channel.detail(terminal: session.origin?.app == "terminal"))
            }
        } else {
            let mode = session.host?.mode ?? ""
            let ready = session.host?.ready == true
            let readyText = ready ? L("托管就绪", "Hosted Ready") : L("托管受阻", "Hosted Blocked")
            tag(mode == "hosted" ? readyText : TrayHostMode.localizedTitle(mode), mode == "hosted" ? (ready ? .green : .orange) : .gray)
            if let job = runtime.lastDelivery(for: session), let channel = DeliveryChannel(job: job), let at = trayDate(job.updated_at), runtime.now.timeIntervalSince(at) < 86400 {
                tag(channel.title(terminal: session.origin?.app == "terminal"), channel.color).help(L("最近一次发送方式：", "Last delivery method: ") + channel.detail(terminal: session.origin?.app == "terminal"))
            }
        }
    }
    private func tag(_ text: String, _ color: Color) -> some View {
        Text(text).font(.caption2.monospacedDigit()).padding(.horizontal, 5).padding(.vertical, 1).background(color.opacity(0.18), in: Capsule())
    }
    private var icon: String {
        switch session.origin?.app { case "claude-desktop": return "macwindow"; case "terminal": return "terminal"; default: return "questionmark.square.dashed" }
    }
}

struct SessionDetail: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    @State private var feedback = ""
    @State private var testing = false
    @State private var cancelling = false
    @State private var locating = false
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                HStack(alignment: .top) {
                    VStack(alignment: .leading, spacing: 5) {
                        Text(session.displayTitle).font(.title2.bold()).textSelection(.enabled)
                        Text(subtitle).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                    }
                    Spacer()
                    Button(locating ? L("定位中…", "Locating…") : L("定位窗口", "Locate Window")) { Task { locating = true; feedback = await runtime.locate(session); locating = false } }.disabled(locating)
                    Button(L("测试发送…", "Test Send…")) { testing = true }.buttonStyle(.borderedProminent).disabled(testBusy)
                    if runtime.job(for: session) != nil { Button(L("停止", "Stop"), role: .destructive) { cancelling = true } }
                }
                if !feedback.isEmpty { Text(feedback).font(.callout).padding(10).frame(maxWidth: .infinity, alignment: .leading).background(.blue.opacity(0.07), in: RoundedRectangle(cornerRadius: 8)) }
                SessionStatusCard(runtime: runtime, session: session)
                SessionTestProgress(runtime: runtime, session: session)
                SessionEditor(runtime: runtime, session: session)
                SessionFacts(runtime: runtime, session: session)
                SessionEvents(runtime: runtime, session: session)
            }.padding(22)
        }
        .sheet(isPresented: $testing) { TestSendSheet(runtime: runtime, session: session, close: { result in testing = false; if let result { feedback = result } }) }
        .confirmationDialog(L("停止这个会话的待处理发送？不会关闭会话，也不会撤回已发送的内容。", "Stop pending send for this session? Will not close the session or revert sent content."), isPresented: $cancelling) {
            Button(L("停止", "Stop"), role: .destructive) { Task { await runtime.cancel(session) } }
            Button(L("返回", "Back"), role: .cancel) {}
        }
    }
    private var subtitle: String {
        var parts = [session.cwd.isEmpty ? L("项目未知", "Unknown project") : session.cwd]
        if let at = runtime.lastActive(session) { parts.append(L("最近活动 ", "Active ") + trayRelative(at, now: runtime.now)) }
        if session.isArchived { parts.append(L("已在 Claude 中归档或删除", "Archived or deleted in Claude")) }
        return parts.joined(separator: " · ")
    }
    private var testBusy: Bool {
        guard let job = runtime.latestTest(for: session) else { return false }
        return ["READY_TO_RESUME", "DISPATCHING", "VERIFYING"].contains(job.state) || ["DISPATCHING", "VERIFYING"].contains(session.state)
    }
}

private struct TestSendSheet: View {
    @ObservedObject var runtime: TrayRuntime
    let session: TraySession
    let close: (String?) -> Void
    @State private var message = ""
    @State private var busy = false
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(L("测试发送到「\(session.displayTitle)」", "Test Send to \"\(session.displayTitle)\"")).font(.headline)
            Text(sendExplanation).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            TextField(effectiveMessage, text: $message).textFieldStyle(.roundedBorder)
            Text(L("留空则发送该会话当前生效的继续文案。", "Leave blank to send this session's currently active continue prompt.")).font(.caption).foregroundStyle(.secondary)
            if session.origin?.app != "terminal" {
                Label(L("点「确认发送」后请松开键盘和鼠标：检测到 3 秒内有操作会自动等待，不会打断你。",
                        "Release keyboard and mouse after clicking Send: automatically pauses if user activity is detected."), systemImage: "hand.raised")
                    .font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
            }
            HStack {
                Button(L("取消", "Cancel")) { close(nil) }.keyboardShortcut(.cancelAction)
                Spacer()
                Button(busy ? L("排队中…", "Queuing…") : L("确认发送", "Send Now")) { Task { busy = true; let result = await runtime.testSend(session, message: message); busy = false; close(result) } }
                    .buttonStyle(.borderedProminent).disabled(busy).keyboardShortcut(.defaultAction)
            }
        }.padding(22).frame(width: 480)
    }
    private var sendExplanation: String {
        guard session.origin?.app == "terminal" else {
            if runtime.directDeliveryEnabled && runtime.directAccepted(session) {
                return L("这会真实地发送一条消息（不检查额度）：会话进程在运行时经会话消息端口静默送达；未运行时先在后台打开该会话让 Claude 启动它，再静默送达，并把焦点还给你原来的应用。进程始终没有启动时才打开会话、核验输入框为空后输入发送。会话随后上报收到消息才算成功。",
                         "Sends a real message (without checking quota): delivered silently through the session socket if running. If not running, wakes session in background first, then delivers silently. Falls back to window typing only if process fails to start. Succeeds once session acknowledges receipt.")
            }
            return L("这会真实地打开该会话（与在 Claude 侧栏点它相同），核验是它本身且输入框为空，再写入文案并按发送；不检查额度。会话随后上报收到消息才算成功。输入框有草稿、窗口对不上时会停下，不会猜。",
                     "Opens the session window (as in Claude sidebar), verifies empty input, types prompt and sends. Halts if draft text exists or window mismatch. Succeeds once session acknowledges receipt.")
        }
        return L("这会真实地发送一条消息（不检查额度）：会话在 tmux 中开着时输入到它的窗格并回车，已关闭时在后台用 claude -p --resume 续写。你正在打字、窗格对不上或会话开着但不在 tmux 中时会停下。",
                 "Sends a real message: types into tmux pane when open; resumes in background with claude -p --resume when closed.")
    }
    private var effectiveMessage: String {
        session.continue_message?.isEmpty == false ? session.continue_message! : (runtime.snapshot?.policy["continue_message"]?.string ?? LanguageManager.shared.defaultContinuePrompt)
    }
}
