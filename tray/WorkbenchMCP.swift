import AppKit
import SwiftUI
import UniformTypeIdentifiers

struct WorkbenchMCP: View {
    @ObservedObject var runtime: TrayRuntime
    @State private var grants: [GuardMCPGrant] = []
    @State private var requests: [GuardMCPRequest] = []
    @State private var selectedScopes: Set<String> = ["read"]
    @State private var selectedTargets = Set<String>()
    @State private var ttl = "3600"
    @State private var busy = false
    @State private var message = ""
    @State private var loadedAt: Date?
    @State private var review: GuardMCPRequest?
    @State private var revoke: GuardMCPGrant?
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 22) {
                Text(L("MCP 授权与用户确认", "MCP Authorization & Approvals")).font(.title.bold())
                Text(L("AI只能读取限定目标、提交待确认意图。只有你在主应用核对精确操作后才能批准；AI自己的ack不是授权。此处使用私有full Bearer管理API，不把主应用令牌导出。",
                       "AI can only read scoped targets and submit pending intent. Operations require manual user review in the main app; AI ack is not authorization. Private full Bearer management API is never exported.")).foregroundStyle(.secondary)
                HStack {
                    Button(busy ? L("处理中…", "Loading…") : L("读取授权与请求", "Reload Grants & Requests")) { Task { await reload() } }.disabled(busy || !runtime.daemonRunning)
                    Text(loadedAt.map { L("列表实际读取：", "List refreshed: ") + $0.formatted(date: .abbreviated, time: .standard) } ?? L("尚未读取列表", "List not loaded yet")).font(.caption).foregroundStyle(.secondary)
                }
                if !message.isEmpty { Text(message).font(.callout).foregroundStyle(.secondary).textSelection(.enabled) }
                issuance
                GroupBox(L("已有授权（不提供再次导出）", "Existing Grants (Export unavailable once issued)")) {
                    VStack(alignment: .leading, spacing: 14) {
                        if grants.isEmpty { Text(L("列表暂无授权；未读取时不表示服务器没有授权。", "No grants listed; unread state does not mean server has no grants.")).font(.caption).foregroundStyle(.secondary) }
                        ForEach(grants) { grant in
                            VStack(alignment: .leading, spacing: 6) {
                                HStack { Text(grant.id).font(.callout.monospaced()); Spacer(); Text(grant.active(at: runtime.now) ? L("有效", "Active") : L("已撤销／到期", "Revoked / Expired")).font(.caption); Button(L("撤销", "Revoke"), role: .destructive) { revoke = grant }.disabled(busy || grant.revoked_at != nil) }
                                Text("scope: " + grant.scopes.joined(separator: ", ")).font(.caption.monospaced())
                                Text(L("精确targets：", "Targets: ") + grant.session_targets.joined(separator: ", ")).font(.caption.monospaced()).textSelection(.enabled)
                                Text(L("创建 ", "Created ") + grant.created_at + L("；到期 ", " · Expires ") + grant.expires_at).font(.caption.monospaced())
                            }.padding(.vertical, 5)
                            Divider()
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
                GroupBox(L("操作请求（不自动批准）", "Operation Requests (Never auto-approved)")) {
                    VStack(alignment: .leading, spacing: 12) {
                        if requests.isEmpty { Text(L("暂无已读取请求。请刷新；不会因AI ack而自动确认。", "No loaded requests. Please refresh; will not auto-confirm from AI ack.")).font(.caption).foregroundStyle(.secondary) }
                        ForEach(requests) { request in
                            HStack(alignment: .top) {
                                VStack(alignment: .leading, spacing: 5) {
                                    Text("\(request.action.operation) → \(request.action.session_id)").font(.headline)
                                    Text("\(request.id) · generation \(request.action.expected_generation) · \(request.state)").font(.caption.monospaced())
                                    Text(L("到期 ", "Expires ") + request.expires_at).font(.caption.monospaced()).foregroundStyle(.secondary)
                                    if let result = request.result {
                                        let okText = result.ok ? L("成功", "Success") : L("未确认成功", "Unconfirmed")
                                        let retractText = result.send_retraction_confirmed ? L("已确认", "Confirmed") : L("未确认", "Unconfirmed")
                                        Text(L("结果：\(okText) / \(result.code)；撤回发送\(retractText)", "Result: \(okText) / \(result.code); Retraction: \(retractText)")).font(.caption)
                                    }
                                }
                                Spacer()
                                Button(request.state == "pending" ? L("核对并决定", "Review & Decide") : L("查看详情", "View Details")) { review = request }.disabled(busy)
                            }.padding(.vertical, 6)
                            Divider()
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
                }
            }.padding(28).frame(maxWidth: 1000, alignment: .leading)
        }.task { await reload() }
            .sheet(item: $review) { request in
                MCPRequestReview(runtime: runtime, request: request, close: { review = nil }, changed: { message = $0; Task { await reload() } })
            }
            .confirmationDialog(L("撤销后此授权的新请求会被拒绝，已提交的外部操作不能保证撤回。", "Revoking rejects new requests from this grant; external actions cannot be guaranteed reverted."), isPresented: Binding(get: { revoke != nil }, set: { if !$0 { revoke = nil } })) {
                Button(L("撤销这一个授权", "Revoke This Grant"), role: .destructive) { if let grant = revoke { Task { await revokeGrant(grant) } }; revoke = nil }
                Button(L("返回", "Cancel"), role: .cancel) { revoke = nil }
            }
    }
    private var issuance: some View {
        GroupBox(L("创建限定授权并仅一次导出0600 profile", "Create Scoped Grant & Export 0600 Profile Once")) {
            VStack(alignment: .leading, spacing: 12) {
                Text(L("导出的JSON只包含daemon_url和access_token。令牌仅此次签发可取，不进入日志、剪贴板或真实用户配置；丢失后撤销重建。", "Exported JSON contains daemon_url and access_token only. Token is only available at issuance.")).font(.caption).foregroundStyle(.secondary)
                ForEach(GuardMCPContract.scopes, id: \.self) { scope in
                    Toggle(scope, isOn: Binding(get: { selectedScopes.contains(scope) }, set: { selected in
                        if selected { selectedScopes.insert(scope) } else { selectedScopes.remove(scope) }
                    })).toggleStyle(.checkbox).font(.caption.monospaced()).disabled(scope == "read" || busy)
                }
                Divider()
                Text(L("限定精确目标（无通配符；daemon为全局策略／刷新目标）", "Specific targets (no wildcards; daemon is global policy target)")).font(.caption)
                Toggle(L("daemon · 全局守护器", "daemon · Global Guard"), isOn: targetSelection("daemon")).toggleStyle(.checkbox).disabled(busy)
                ScrollView {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(runtime.snapshot?.sessions ?? []) { session in
                            Toggle(isOn: targetSelection(session.id)) { VStack(alignment: .leading) { Text(session.title.isEmpty ? session.id : session.title); Text(session.id).font(.caption.monospaced()).foregroundStyle(.secondary) } }.toggleStyle(.checkbox).disabled(busy)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }.frame(maxHeight: 170)
                HStack {
                    Text(L("TTL秒（60–86400）", "TTL seconds (60-86400)"))
                    TextField("TTL", text: $ttl).textFieldStyle(.roundedBorder).frame(width: 130).disabled(busy)
                    Text(L("已选 \(selectedTargets.count) 个targets", "\(selectedTargets.count) target(s) selected")).font(.caption)
                }
                Button(L("确认以上scope、targets与TTL，创建并导出新profile…", "Confirm Scope, Targets & TTL, Create & Export Profile…")) { Task { await issue() } }.buttonStyle(.borderedProminent).disabled(busy || selectedTargets.isEmpty || !runtime.desktopExecutionReady)
            }.frame(maxWidth: .infinity, alignment: .leading).padding(10)
        }
    }
    private func targetSelection(_ id: String) -> Binding<Bool> {
        Binding(get: { selectedTargets.contains(id) }, set: { selected in if selected { selectedTargets.insert(id) } else { selectedTargets.remove(id) } })
    }
    @MainActor private func reload() async {
        guard !busy else { return }; busy = true; defer { busy = false }
        do {
            let readGrants = try await runtime.controlGrants()
            let readRequests = try await runtime.controlRequests()
            grants = readGrants; requests = readRequests; loadedAt = Date()
        } catch { message = (error as? RuntimeFailure)?.message ?? "列表读取未完成；保留旧列表和真实读取时间，不标记为最新" }
    }
    @MainActor private func issue() async {
        guard !busy, let seconds = Int(ttl) else { message = "请填写整数TTL"; return }
        let scopes = selectedScopes, targets = selectedTargets
        do { _ = try GuardMCPContract.createGrant(scopes: scopes, targets: targets, ttl: seconds) }
        catch { message = (error as? RuntimeFailure)?.message ?? "授权字段无效"; return }
        busy = true
        let panel = NSSavePanel()
        panel.title = "导出一次性 scoped profile（不能覆盖现有文件）"
        panel.nameFieldStringValue = "quota-guard-scoped-profile.json"
        panel.allowedContentTypes = [.json]
        panel.canCreateDirectories = false
        let response = await withCheckedContinuation { continuation in panel.begin { continuation.resume(returning: $0) } }
        guard response == .OK, let url = panel.url else { busy = false; return }
        do {
            let id = try await runtime.createGrantAndExport(scopes: scopes, targets: targets, ttl: seconds, to: url)
            message = "授权 \(id) 已创建，0600 profile已导出一次。令牌不再保留；不会自动安装MCP或修改任何客户端配置。"
        } catch { message = (error as? RuntimeFailure)?.message ?? "签发或导出未确认，请读取授权记录；不重复签发、不打印令牌" }
        busy = false
        await reload()
    }
    @MainActor private func revokeGrant(_ grant: GuardMCPGrant) async {
        guard !busy else { return }; busy = true
        do { try await runtime.revokeControlGrant(grant.id); message = "此授权撤销已确认；不保证已发生操作能撤回。" }
        catch { message = (error as? RuntimeFailure)?.message ?? "撤销未确认，请刷新授权记录" }
        busy = false
        await reload()
    }
}

struct MCPRequestReview: View {
    @ObservedObject var runtime: TrayRuntime
    let request: GuardMCPRequest
    let close: () -> Void
    let changed: (String) -> Void
    @State private var confirmed = false
    @State private var busy = false
    @State private var error = ""
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("用户核对精确操作请求").font(.headline)
            ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                    LabeledContent("操作原名", value: request.action.operation)
                    LabeledContent("精确目标", value: request.action.session_id)
                    LabeledContent("generation", value: String(request.action.expected_generation))
                    LabeledContent("request_id", value: request.id)
                    LabeledContent("grant_id", value: request.grant_id)
                    Text("digest：\(request.digest)").font(.caption.monospaced()).textSelection(.enabled)
                    if let policy = request.action.policy_digest { Text("policy_digest：\(policy)").font(.caption.monospaced()).textSelection(.enabled) }
                    Text("创建：\(request.created_at)\n到期：\(request.expires_at)").font(.caption.monospaced())
                    Text("剩余TTL：" + (trayDate(request.expires_at).map { trayDuration($0.timeIntervalSince(runtime.now)) } ?? "未知"))
                    Text("精确参数").font(.headline)
                    Text(request.action.exactParameters).font(.caption.monospaced()).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                    Text("状态：\(request.state)。取消不能保证撤回已发送操作；enable_auto_resume可能允许后续自动发送；settingsPatch只修改列出的字段，不代表有发送权。").font(.caption).foregroundStyle(.secondary)
                    if let problem = runtime.controlApprovalProblem(request) { Text(problem).font(.caption).foregroundStyle(.orange) }
                }.padding(4)
            }.frame(maxHeight: 390)
            if request.state == "pending" { Toggle("我已核对以上精确操作、targets、generation、digest和TTL，并由我本人决定", isOn: $confirmed).toggleStyle(.checkbox).disabled(busy) }
            if !error.isEmpty { Text(error).font(.caption).foregroundStyle(.orange) }
            HStack {
                Button("返回", action: close).disabled(busy)
                Spacer()
                if request.state == "pending" {
                    Button("拒绝", role: .destructive) { Task { await decide(false) } }.disabled(busy || !confirmed || !request.confirmable(at: runtime.now))
                    Button("批准这个精确请求") { Task { await decide(true) } }.buttonStyle(.borderedProminent).disabled(busy || !confirmed || runtime.controlApprovalProblem(request) != nil)
                }
            }
        }.padding(24).frame(width: 620)
    }
    @MainActor private func decide(_ approve: Bool) async {
        guard !busy, confirmed else { return }; busy = true; defer { busy = false }
        do {
            let result = try await runtime.decideControlRequest(request, approve: approve, userConfirmed: confirmed)
            changed("用户决定已记录：\(result.state)。" + (result.result.map { "结果：\($0.code)。" } ?? "尚无执行结果。"))
            close()
        } catch {
            confirmed = false
            self.error = (error as? RuntimeFailure)?.message ?? "决定结果未确认；请关闭并重新读取，不自动重试"
        }
    }
}
