import SwiftUI

private struct AccountPick: Identifiable { let id: String }

struct WorkbenchResources: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var search = ""
    @State private var provider = ""
    @State private var picked: AccountPick?
    @AppStorage("QSOQuotaLayout") private var layout = "grid"
    private var accounts: [TrayAccount] { runtime.snapshot?.accounts ?? [] }
    private var filtered: [TrayAccount] {
        accounts.filter { (provider.isEmpty || $0.provider == provider) && (search.isEmpty || "\($0.email) \($0.provider) \($0.plan) \($0.id) \(($0.groups ?? []).map(\.name).joined(separator: " "))".localizedCaseInsensitiveContains(search)) }
            .sorted { rank($0) != rank($1) ? rank($0) < rank($1) : $0.email < $1.email }
    }
    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                TextField("搜索邮箱、套餐或额度组", text: $search).textFieldStyle(.roundedBorder)
                Picker("供应商", selection: $provider) {
                    Text("全部供应商").tag("")
                    ForEach(Array(Set(accounts.map(\.provider))).sorted(), id: \.self) { Text(ProviderStyle($0).name).tag($0) }
                }.frame(width: 180)
                Picker("布局", selection: $layout) {
                    Image(systemName: "square.grid.2x2").help("平铺").tag("grid")
                    Image(systemName: "list.bullet").help("列表").tag("list")
                }.pickerStyle(.segmented).labelsHidden().frame(width: 84)
            }.padding(.horizontal, 18).padding(.vertical, 12)
            summaryStrip.padding(.horizontal, 18).padding(.bottom, 12)
            Divider()
            if accounts.isEmpty {
                WorkbenchEmpty(title: "还没有额度账号", detail: "先连接 CLIProxyAPI 并刷新；未知的数据不会显示成 0%。", icon: "chart.bar")
                Spacer()
            } else if filtered.isEmpty {
                WorkbenchEmpty(title: "没有匹配的账号", detail: "调整搜索词或供应商筛选。", icon: "magnifyingglass")
                Spacer()
            } else if layout == "list" {
                QuotaTable(runtime: runtime, accounts: filtered) { picked = AccountPick(id: $0) }
            } else {
                ScrollView {
                    MasonryLayout(minColumnWidth: 300, spacing: 14) {
                        ForEach(filtered) { account in QuotaTile(runtime: runtime, account: account) { picked = AccountPick(id: account.id) } }
                    }.padding(18)
                }.background(.quaternary.opacity(0.25))
            }
        }
        .sheet(item: $picked) { pick in
            if let account = accounts.first(where: { $0.id == pick.id }) {
                QuotaAccountDetail(runtime: runtime, account: account) { picked = nil }
            } else {
                VStack(spacing: 12) { Text("该账号已不在来源中").font(.headline); Button("关闭") { picked = nil } }.padding(30)
            }
        }
    }
    // Limited first, then unknown, then available; disabled last.
    private func rank(_ account: TrayAccount) -> Int {
        if account.disabled == true { return 4 }
        if !runtime.isFresh(account) { return 1 }
        return account.state == "limited" ? 0 : account.state == "allowed" ? 2 : 1
    }
    private var summaryStrip: some View {
        let live = accounts.filter { $0.disabled != true }
        let available = live.filter { runtime.isFresh($0) && $0.state == "allowed" }.count
        let limited = live.filter { runtime.isFresh($0) && $0.state == "limited" }.count
        let unknown = live.count - available - limited
        let bound = Set(runtime.index.sessionsByAccount.values.flatMap { $0.map(\.id) }).count
        let soonest = live.filter { $0.state == "limited" }.compactMap { trayDate($0.available_at) }.filter { $0 > runtime.now }.min()
        return HStack(spacing: 10) {
            stat("账号", "\(live.count)", icon: "person.crop.circle", color: .accentColor)
            stat("可用", "\(available)", icon: "checkmark.circle.fill", color: .green)
            stat("受限", "\(limited)", icon: "xmark.octagon.fill", color: limited > 0 ? .red : .secondary)
            stat("未知 / 等待复查", "\(unknown)", icon: "questionmark.circle", color: unknown > 0 ? .orange : .secondary)
            stat("绑定会话", "\(bound)", icon: "bubble.left.and.bubble.right", color: .accentColor)
            stat("最早恢复", soonest.map { trayDuration($0.timeIntervalSince(runtime.now)) } ?? "—", icon: "clock", color: soonest == nil ? .secondary : .blue)
        }
    }
    private func stat(_ title: String, _ value: String, icon: String, color: Color) -> some View {
        HStack(spacing: 8) {
            Image(systemName: icon).foregroundStyle(color).font(.title3)
            VStack(alignment: .leading, spacing: 1) {
                Text(value).font(.headline.monospacedDigit()).lineLimit(1)
                Text(title).font(.caption2).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer(minLength: 0)
        }.padding(.horizontal, 10).padding(.vertical, 7).frame(maxWidth: .infinity).background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 9))
    }
}

// Everything about one account: every window, the evidence, and the sessions that depend on it.
struct QuotaAccountDetail: View {
    @ObservedObject var runtime: TrayRuntime
    let account: TrayAccount
    let close: () -> Void
    var body: some View {
        let sessions = runtime.sessions(boundTo: account.id)
        VStack(spacing: 0) {
            HStack(spacing: 10) {
                ProviderIcon(provider: account.provider, size: 34)
                VStack(alignment: .leading, spacing: 2) {
                    Text(account.email.isEmpty ? account.id : account.email).font(.headline)
                    Text(ProviderStyle(account.provider).name + " · " + (account.plan.isEmpty ? "套餐未知" : account.plan)).font(.caption).foregroundStyle(.secondary)
                }
                Spacer()
                Button("完成", action: close).keyboardShortcut(.defaultAction)
            }.padding(16)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    TrayQuotaCard(runtime: runtime, account: account)
                    GroupBox("绑定的会话（\(sessions.count)）") {
                        VStack(alignment: .leading, spacing: 8) {
                            if sessions.isEmpty { Text("没有会话按模型或手动设置使用这个账号。").foregroundStyle(.secondary) }
                            ForEach(sessions) { session in
                                HStack(spacing: 8) {
                                    Circle().fill(stateColor(session.state)).frame(width: 7, height: 7)
                                    Text(session.displayTitle).lineLimit(1)
                                    Spacer()
                                    Text(TrayHostMode.title(session.host?.mode ?? "")).font(.caption).foregroundStyle(.secondary)
                                    Text(trayState(session.state)).font(.caption).foregroundStyle(stateColor(session.state))
                                }
                            }
                            if !sessions.isEmpty { Button("打开会话托管") { close(); runtime.openWorkbench(.sessions) }.controlSize(.small) }
                        }.font(.callout).frame(maxWidth: .infinity, alignment: .leading).padding(6)
                    }
                    GroupBox("证据与适用范围") {
                        VStack(alignment: .leading, spacing: 8) {
                            Text("来源标识：" + (account.source_id ?? "兼容默认 CPA"))
                            Text("查询时间：" + (trayDate(account.checked_at)?.formatted(date: .abbreviated, time: .standard) ?? "未知"))
                            if let next = trayDate(account.next_check_at) { Text("下次复查：" + next.formatted(date: .abbreviated, time: .standard)) }
                            Text("模型：" + ((account.models ?? []).isEmpty ? "来源尚未提供；不猜模型映射" : (account.models ?? []).joined(separator: "、")))
                            Text("额度数据来自上游官方用量接口（经 CLIProxyAPI 转发）。查询本身被限流（429）时会退避重试，不当作额度耗尽；离重置还早时也会定期复查，以便发现提前重置。").foregroundStyle(.secondary)
                        }.font(.callout).frame(maxWidth: .infinity, alignment: .leading).padding(6)
                    }
                }.padding(18)
            }
        }.frame(minWidth: 560, idealWidth: 620, minHeight: 520, idealHeight: 640)
    }
    private func stateColor(_ state: String) -> Color {
        switch state {
        case "NEEDS_ATTENTION": return .orange
        case "COOLDOWN", "READY_TO_RESUME", "VERIFYING", "DISPATCHING": return .blue
        case "RUNNING": return .green
        default: return .secondary
        }
    }
}

struct WorkbenchEmpty: View {
    let title: String
    let detail: String
    let icon: String
    var body: some View {
        VStack(spacing: 15) { Image(systemName: icon).font(.system(size: 36)).foregroundStyle(.secondary); Text(title).font(.title3.bold()); Text(detail).foregroundStyle(.secondary).multilineTextAlignment(.center).frame(maxWidth: 400) }.padding(35).frame(maxWidth: .infinity, minHeight: 260)
    }
}

func workbenchReason(_ reason: String) -> String {
    let values = ["prompt submitted": "已收到会话提交事件", "session started": "已接入会话事件", "rate limit": "本回合额度受限，等待新鲜额度证据", "normal stop": "本回合已正常结束，不自动继续", "no explicit binding": "尚未绑定恢复来源；可先保存监控组合", "auto-resume is disabled": "只监控，未授权自动恢复", "unknown or stale quota evidence": "额度证据未知或过期，不能据此发送", "waiting for quota reset": "等待额度复查；到达重置时间不代表已恢复", "account concurrency limit": "等待该来源的恢复预算", "only code sessions can auto-resume": "自动恢复仅支持已确认的 Code 会话", "quota request returned HTTP 429": "额度查询被上游限流（429），已退避；不视为额度耗尽"]
    if reason.hasPrefix("waiting for desktop:") { return "等待桌面就绪：检查 Claude、锁屏与辅助功能；不会盲发继续。" }
    if reason.contains("HTTP 429") { return "额度查询被上游限流（429），已退避；不视为额度耗尽" }
    return values[reason] ?? reason
}
