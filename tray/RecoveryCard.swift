import SwiftUI

// One recovery task: a hero countdown, the six-step pipeline, every clock the
// daemon keeps for it, the pool's accounts and the latest events. Re-renders
// every second from runtime.now; nothing here reads the clock on its own.
struct RecoveryCard: View {
    @ObservedObject var runtime: TrayRuntime
    let task: RecoveryTask
    @State private var feedback = ""
    @State private var cancelling = false
    @State private var locating = false
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            header
            hero
            RecoveryPipelineView(pipeline: task.pipeline, details: stepDetails)
            clocks
            if let block = task.clock?.block, block != .none, task.job.active { callout(task.clock?.blockText ?? "", icon: "pause.circle.fill", color: .orange) }
            if task.job.state == "NEEDS_ATTENTION" { callout(task.job.reason_text ?? task.job.reason ?? "需要人工查看", icon: "exclamationmark.triangle.fill", color: .red) }
            pool
            events
            if !feedback.isEmpty { Text(feedback).font(.caption).foregroundStyle(.secondary) }
        }
        .padding(16).frame(maxWidth: .infinity, alignment: .leading)
        .background(.background, in: RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(accent.opacity(0.35), lineWidth: 1))
        .confirmationDialog("停止「\(task.title)」的自动继续？不会关闭会话，也不会撤回已发送的内容。", isPresented: $cancelling) {
            Button("停止", role: .destructive) { if let session = task.session { Task { await runtime.cancel(session) } } }
            Button("返回", role: .cancel) {}
        }
    }

    private var now: Date { runtime.now }
    private var channel: DeliveryChannel? { DeliveryChannel(job: task.job) }
    private var terminal: Bool { task.session?.origin?.app == "terminal" }
    private var accent: Color {
        switch task.job.state {
        case "NEEDS_ATTENTION": return .red
        case "COMPLETED": return .green
        case "CANCELLED": return .gray
        case "DISPATCHING", "VERIFYING", "READY_TO_RESUME": return .blue
        default: return task.clock?.block == RecoveryBlock.none ? .indigo : .orange
        }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: stateIcon).font(.title2).foregroundStyle(accent).frame(width: 28)
            VStack(alignment: .leading, spacing: 3) {
                Text(task.title).font(.headline).lineLimit(1).textSelection(.enabled)
                Text(subtitle).font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer()
            QuotaStateBadge(text: stateTitle, color: accent)
            QuotaStateBadge(text: task.hosted ? "托管·自动继续" : "仅监控", color: task.hosted ? .blue : .secondary)
            if task.hosted || channel != nil { DeliveryBadge(runtime: runtime, session: task.session, job: task.job) }
            if task.job.active, let session = task.session {
                Button(locating ? "定位中…" : "定位") { Task { locating = true; feedback = await runtime.locate(session); locating = false } }
                    .controlSize(.small).disabled(locating).help("把该会话带到前台，不写入任何内容")
                Button("停止", role: .destructive) { cancelling = true }.controlSize(.small)
            }
            if let session = task.session {
                Button { runtime.focusSession = session.id; runtime.openWorkbench(.sessions) } label: { Image(systemName: "arrow.up.forward.square") }
                    .buttonStyle(.borderless).help("在「会话托管」中打开")
            }
        }
    }
    private var subtitle: String {
        var parts: [String] = []
        if let project = task.session?.projectName, !project.isEmpty { parts.append(project) }
        if let app = task.session?.origin?.appTitle { parts.append(app) }
        if let model = task.session?.model, !model.isEmpty { parts.append(model) }
        parts.append("第 \(task.job.generation ?? 0) 轮")
        return parts.joined(separator: " · ")
    }
    // What a "waiting for desktop" pause is actually waiting for.
    private var desktopWait: (badge: String, hero: String) {
        let code = (task.job.reason ?? "").replacingOccurrences(of: "waiting for desktop: ", with: "").split(separator: " ").first.map(String.init) ?? ""
        switch code {
        case "claude_in_use": return ("等你离开 Claude", "等 Claude 空闲")
        case "user_active": return ("等你停手", "等键鼠空闲")
        case "screen_locked", "screen_inactive", "console_inactive": return ("等解锁", "等解锁")
        case "session_busy": return ("会话忙", "等会话空闲")
        default: return ("等桌面就绪", "等桌面")
        }
    }
    private var stateIcon: String {
        switch task.job.state {
        case "COMPLETED": return "checkmark.seal.fill"
        case "CANCELLED": return "stop.circle"
        case "NEEDS_ATTENTION": return "exclamationmark.triangle.fill"
        case "DISPATCHING": return "paperplane.circle.fill"
        case "VERIFYING": return "checkmark.message.fill"
        case "READY_TO_RESUME": return "checkmark.circle.fill"
        default: return task.clock?.block == RecoveryBlock.none ? "hourglass.circle.fill" : "pause.circle.fill"
        }
    }
    private var stateTitle: String {
        switch task.job.state {
        case "COMPLETED": return "已继续"
        case "CANCELLED": return "已停止"
        case "COOLDOWN":
            switch task.clock?.block ?? .none {
            case .concurrency: return "等并发名额"
            case .desktop: return desktopWait.badge
            case .nativeGrace: return "宽限期"
            case .none: return (task.clock?.available ?? 0) > 0 ? "复查确认中" : "等待额度"
            }
        default: return trayState(task.job.state)
        }
    }

    // The single number that matters most for this task, large.
    private var hero: some View {
        let (label, value, detail) = heroText
        let progress = heroProgress
        return VStack(alignment: .leading, spacing: 6) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                Text(label).font(.callout).foregroundStyle(.secondary)
                Text(value).font(.system(size: 34, weight: .semibold, design: .rounded).monospacedDigit()).foregroundStyle(accent)
                    .contentTransition(.numericText()).lineLimit(1).minimumScaleFactor(0.6)
                Spacer()
                Text(detail).font(.caption).foregroundStyle(.secondary).multilineTextAlignment(.trailing)
            }
            if let progress { ProgressView(value: progress).tint(accent) }
        }
    }
    private var heroText: (String, String, String) {
        let job = task.job
        switch job.state {
        case "COMPLETED":
            var waited = "—"
            if let start = task.started, let end = task.finished { waited = trayDuration(end.timeIntervalSince(start)) }
            return ("已于", recoveryTime(task.finished, now: now), "共等待 " + waited + (task.finished.map { " · " + trayRelative($0, now: now) } ?? ""))
        case "CANCELLED": return ("已停止于", recoveryTime(task.finished, now: now), job.reason_text ?? "")
        case "NEEDS_ATTENTION": return ("需要处理", "不会自动重发", "请在会话里确认后手动继续")
        case "DISPATCHING", "VERIFYING": return ("进行中", job.state == "DISPATCHING" ? "正在发送" : "等待回执", "已用 " + since(trayDate(job.updated_at)))
        default: break
        }
        guard let clock = task.clock else { return ("预计继续", "—", "会话信息缺失") }
        switch clock.block {
        case .concurrency: return ("预计继续", "等名额", "额度已可用 · 占用会话结束后立即继续")
        case .desktop: return ("预计继续", desktopWait.hero, "解除后自动重试 · " + countdown(clock.nextAttempt, due: "即将重试"))
        case .nativeGrace: return ("宽限期剩余", countdown(clock.nextAttempt, due: "即将接手"), "随后由 LimitBreak 接手")
        case .none: break
        }
        if job.state == "READY_TO_RESUME" { return ("预计继续", "排队发送", "额度已确认") }
        if clock.available > 0 { return ("预计继续", "复查后", "池中有账号可用，等复查确认") }
        guard let at = clock.expected else { return ("预计继续", "未知", clock.total == 0 ? "没有可等待的账号" : "来源未给出重置时间，按节奏复查") }
        let title = task.hosted ? "距自动继续" : "距额度恢复"
        return (title, countdown(at, due: clock.fromRecordedReset ? "即将尝试" : "复查确认中"),
                at.formatted(.dateTime.weekday(.abbreviated).hour().minute().second()) + (clock.fromRecordedReset ? "\n查询被限流，按记录的重置时间" : "\n以复查到的新额度为准"))
    }
    // Elapsed share of the wait from hitting the limit to the expected reset.
    private var heroProgress: Double? {
        guard task.job.active, task.clock?.block == RecoveryBlock.none, let start = task.started, let end = task.clock?.expected, end > start else { return nil }
        return min(1, max(0, now.timeIntervalSince(start) / end.timeIntervalSince(start)))
    }

    private var clocks: some View {
        let clock = task.clock
        let job = task.job
        return LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), spacing: 8)], spacing: 8) {
            cell("撞上限额", recoveryTime(task.started, now: now), task.started.map { trayRelative($0, now: now) } ?? "", "exclamationmark.octagon", .red)
            cell(task.finished == nil ? "已等待" : "共等待", task.started.map { trayDuration((task.finished ?? now).timeIntervalSince($0)) } ?? "—", "从撞限额起计", "timer", .indigo)
            if job.active {
                cell("下次查询额度", countdown(clock?.nextCheck, due: "查询中…"), clock?.nextCheck.map { recoveryTime($0, now: now) } ?? "按来源节奏查询", "arrow.triangle.2.circlepath", .teal)
                cell("下次评估", clock?.block == .concurrency ? "名额释放即试" : countdown(clock?.nextAttempt, due: "评估中…"), clock?.nextAttempt.map { recoveryTime($0, now: now) } ?? "", "clock.arrow.circlepath", .blue)
                cell("最早重置", countdown(clock?.expected, due: "已到点"), clock?.expected.map { recoveryTime($0, now: now) } ?? (clock.map { $0.available > 0 ? "已有可用账号" : "未知" } ?? ""), "clock.badge.checkmark", .green)
                cell("额度池", clock.map { $0.total == 0 ? "未匹配" : "\($0.available)/\($0.total) 可用" } ?? "—", task.session?.pool?.basisTitle ?? "", "person.2.circle", (clock?.available ?? 0) > 0 ? .green : .orange)
            }
            cell("发送次数", "\(job.attempts ?? 0)", (job.attempts ?? 0) > 0 ? "不确定时不会重发" : "尚未发送", "paperplane", .secondary)
            if let channel {
                cell("发送方式", channel.title(terminal: terminal), channel.summary(terminal: terminal), channel.icon, channel.color)
                    .help(channel.detail(terminal: terminal))
            } else if task.hosted, job.active, let session = task.session {
                let plan = runtime.deliveryPlan(for: session)
                cell("将使用", plan.title, plan.summary, plan.icon, plan.color).help(plan.detail)
            }
            if let account = job.account_id, !account.isEmpty { cell("使用账号", shortAccount(account), "", "person.crop.circle", .secondary) }
        }
    }
    private func cell(_ title: String, _ value: String, _ detail: String, _ icon: String, _ color: Color) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Label(title, systemImage: icon).font(.caption).foregroundStyle(.secondary).labelStyle(.titleAndIcon).lineLimit(1)
            Text(value).font(.body.weight(.semibold).monospacedDigit()).foregroundStyle(color == .secondary ? Color.primary : color).lineLimit(1).minimumScaleFactor(0.7)
            if !detail.isEmpty { Text(detail).font(.caption2).foregroundStyle(.tertiary).lineLimit(1) }
        }.padding(8).frame(maxWidth: .infinity, minHeight: 58, alignment: .topLeading)
            .background(color.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder private var pool: some View {
        let accounts = runtime.poolAccounts(task.session)
        if task.job.active, !accounts.isEmpty {
            VStack(alignment: .leading, spacing: 6) {
                Text("账号池（任一账号恢复即可继续）").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                ForEach(accounts) { account in
                    let badge = runtime.accountBadge(account)
                    let holders = runtime.sessions(boundTo: account.id).filter { $0.id != task.job.session_id && $0.state == "RUNNING" }
                    HStack(spacing: 8) {
                        ProviderIcon(provider: account.provider, size: 20)
                        Text(account.email.isEmpty ? shortAccount(account.id) : account.email).font(.callout).lineLimit(1)
                        QuotaStateBadge(text: badge.0, color: badge.1)
                        if !holders.isEmpty {
                            Text("运行中：" + holders.prefix(2).map(\.displayTitle).joined(separator: "、")).font(.caption2).foregroundStyle(.orange).lineLimit(1)
                        }
                        Spacer()
                        if let reset = runtime.accountReset(account) {
                            Label(countdown(reset, due: "已到点"), systemImage: "clock.badge.checkmark").font(.caption.monospacedDigit()).foregroundStyle(.green)
                                .help("重置于 " + reset.formatted(date: .abbreviated, time: .standard))
                        }
                        if let next = trayDate(account.next_check_at), next > now {
                            Label(trayDuration(next.timeIntervalSince(now)), systemImage: "arrow.triangle.2.circlepath").font(.caption.monospacedDigit()).foregroundStyle(.teal).help("下次查询该账号额度")
                        }
                    }
                }
            }.padding(10).background(.quaternary.opacity(0.3), in: RoundedRectangle(cornerRadius: 9))
        }
    }

    @ViewBuilder private var events: some View {
        let items = task.session.map { runtime.events(for: $0) } ?? []
        if !items.isEmpty {
            DisclosureGroup("最近事件（\(items.count)）") {
                VStack(alignment: .leading, spacing: 4) {
                    ForEach(items) { item in
                        HStack(alignment: .top, spacing: 8) {
                            Text(recoveryTime(trayDate(item.time), now: now)).font(.caption.monospacedDigit()).foregroundStyle(.secondary).frame(width: 78, alignment: .leading)
                            Text(item.display).font(.caption).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
                        }
                    }
                }.padding(.top, 4)
            }.font(.caption)
        }
    }

    private var stepDetails: [String] {
        let clock = task.clock
        let job = task.job
        return [
            recoveryTime(task.started, now: now),
            clock?.expected.map { countdown($0, due: "已到点") } ?? (job.active ? "按节奏复查" : ""),
            job.active ? (clock?.nextCheck.map { "查询 " + countdown($0, due: "进行中") } ?? "") : "",
            channel?.title(terminal: terminal) ?? (clock?.block == RecoveryBlock.none || !job.active ? ((job.attempts ?? 0) > 0 ? "已发 \(job.attempts ?? 0) 次" : "") : "已暂停"),
            job.state == "VERIFYING" ? "等 Hook 确认" : "",
            job.state == "COMPLETED" ? recoveryTime(task.finished, now: now) : "",
        ]
    }
    private func callout(_ text: String, icon: String, color: Color) -> some View {
        Label(text, systemImage: icon).font(.callout).foregroundStyle(color).fixedSize(horizontal: false, vertical: true)
            .padding(10).frame(maxWidth: .infinity, alignment: .leading).background(color.opacity(0.08), in: RoundedRectangle(cornerRadius: 8))
    }
    private func countdown(_ date: Date?, due: String) -> String {
        guard let date else { return "—" }
        return date > now ? trayDuration(date.timeIntervalSince(now)) : due
    }
    private func since(_ date: Date?) -> String { date.map { trayDuration(now.timeIntervalSince($0)) } ?? "—" }
    private func shortAccount(_ id: String) -> String {
        let name = id.replacingOccurrences(of: ".json", with: "")
        return name.count > 28 ? String(name.prefix(28)) + "…" : name
    }
}

// Six connected steps; the active one pulses with a spinner, a paused one shows why.
struct RecoveryPipelineView: View {
    let pipeline: RecoveryPipeline
    let details: [String]
    var body: some View {
        HStack(alignment: .top, spacing: 0) {
            ForEach(0..<RecoveryPipeline.titles.count, id: \.self) { step in
                let status = pipeline.status(step)
                VStack(spacing: 4) {
                    HStack(spacing: 0) {
                        Rectangle().fill(step == 0 ? Color.clear : line(pipeline.status(step - 1), status)).frame(height: 2)
                        marker(status, icon: RecoveryPipeline.icons[step])
                        Rectangle().fill(step == RecoveryPipeline.titles.count - 1 ? Color.clear : line(status, pipeline.status(step + 1))).frame(height: 2)
                    }
                    Text(RecoveryPipeline.titles[step]).font(.caption.weight(status == .pending ? .regular : .semibold))
                        .foregroundStyle(status == .pending ? Color.secondary : Color.primary).lineLimit(1).minimumScaleFactor(0.8)
                    Text(step < details.count ? details[step] : "").font(.caption2.monospacedDigit()).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.7)
                }.frame(maxWidth: .infinity)
            }
        }
    }
    private func line(_ from: RecoveryPipeline.Status, _ to: RecoveryPipeline.Status) -> Color { from == .done ? .green.opacity(0.6) : .secondary.opacity(0.25) }
    @ViewBuilder private func marker(_ status: RecoveryPipeline.Status, icon: String) -> some View {
        ZStack {
            Circle().fill(color(status).opacity(status == .pending ? 0.08 : 0.16)).frame(width: 30, height: 30)
            switch status {
            case .done: Image(systemName: "checkmark").font(.caption.bold()).foregroundStyle(.green)
            case .active: ProgressView().controlSize(.small)
            case .waiting: Image(systemName: "pause.fill").font(.caption).foregroundStyle(.orange)
            case .failed: Image(systemName: "xmark").font(.caption.bold()).foregroundStyle(.red)
            case .cancelled: Image(systemName: "stop.fill").font(.caption).foregroundStyle(.gray)
            case .pending: Image(systemName: icon).font(.caption).foregroundStyle(.tertiary)
            }
        }
    }
    private func color(_ status: RecoveryPipeline.Status) -> Color {
        switch status {
        case .done: return .green
        case .active: return .blue
        case .waiting: return .orange
        case .failed: return .red
        case .cancelled, .pending: return .gray
        }
    }
}
