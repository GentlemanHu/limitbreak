package domain

import (
	"fmt"
	"strings"
)

// Engine reasons stay stable English identifiers for tests and audit; the UI
// shows Explain(reason). Newer reasons are already Chinese and pass through.
var explanations = []struct{ prefix, text string }{
	{"atomic desktop and resource lease acquired", "已锁定目标与账号，正在发送"},
	{"binding changed during dispatch", "发送过程中绑定被修改，需人工确认"},
	{"binding changed", "绑定已修改"},
	{"confirmed by ", "已由会话 Hook 确认发送成功"},
	{"dispatch confirmation timed out", "发送后未收到会话确认；为避免重复不会再发，请人工查看"},
	{"hook model differs from explicit binding", "会话模型与绑定不一致，需更新绑定"},
	{"hook model differs from binding", "会话模型与绑定不一致"},
	{"model changed; update explicit binding", "模型已切换，需更新旧版绑定"},
	{"model changed; binding must be revalidated", "模型已切换"},
	{"native quota auto-resume disabled", "原生自动继续已关闭"},
	{"native quota auto-resume fired", "原生自动继续已触发，会话已恢复"},
	{"native quota auto-resume is stale", "原生自动继续已失效"},
	{"native quota auto-resume requires", "原生自动继续状态需确认"},
	{"native quota notification superseded", "原生通知取代了本次排队"},
	{"non-quota StopFailure", "非额度原因中断，需人工查看"},
	{"normal stop", "已正常结束本轮"},
	{"obsolete generation", "已过期（会话有新动态）"},
	{"policy changed during dispatch", "发送过程中设置被修改，需人工确认"},
	{"prompt submitted", "运行中"},
	{"rate limit", "额度耗尽，排队等待恢复"},
	{"running lifecycle is stale", "长时间无运行信号，需确认会话状态"},
	{"session ended", "会话已结束"},
	{"session started", "会话已开始"},
	{"submission failed or uncertain", "发送失败或结果不确定；不会自动重发，请人工查看"},
	{"submitted; waiting for a fresh hook", "已发送，等待会话确认"},
	{"submitted via direct channel; waiting", "已后台静默直发，会话已开始处理，等待会话确认"},
	{"submitted via direct channel, turn not started", "已后台静默直发，但会话尚未开始处理；若该会话为「跳过权限确认」模式，请在会话里批准这条消息。不会自动重发"},
	{"submitted via direct channel after waking the session; waiting", "已在后台唤醒会话并静默直发，会话已开始处理，等待会话确认"},
	{"submitted via direct channel after waking the session, turn not started", "已在后台唤醒会话并静默直发，但会话尚未开始处理；若该会话为「跳过权限确认」模式，请在会话里批准这条消息。不会自动重发"},
	{"submitted via the Claude window", "已通过 Claude 窗口发送（会话进程未能在后台启动，改用界面发送），等待会话确认"},
	{"waiting for desktop: ", "等待桌面可用（锁屏、权限或目标窗口未就绪）"},
	{"account source identity changed", "账号来源变化，需重新绑定"},
	{"waiting for quota reset", "等待额度重置"},
	{"unknown or stale quota evidence", "额度数据过期，等待刷新"},
	{"account concurrency limit", "账号并发已满，排队中"},
	{"quota ready", "额度可用"},
	{"auto-resume is disabled", "旧版绑定未开启自动继续"},
	{"binding has no explicit resource pool", "旧版绑定缺少账号池"},
	{"desktop target is missing", "旧版绑定缺少目标"},
	{"invalid resume binding", "旧版绑定无效"},
	{"native auto-resume disablement", "旧版绑定未确认关闭原生自动继续"},
	{"no explicit binding", "未绑定"},
	{"only code sessions can auto-resume", "仅 Code 会话可自动继续"},
	{"resume ownership is native", "旧版绑定为仅监控"},
	{"session model differs from binding", "会话模型与旧版绑定不一致"},
	{"side effects have not been acknowledged", "旧版绑定未确认副作用"},
	{"automatic resume hourly circuit breaker", "1 小时内自动继续次数达到上限，已暂停该会话"},
	{"new prompt superseded", "用户已手动继续"},
	{"new session lifecycle", "会话重新开始"},
	{"failure superseded", "新的中断取代了旧排队"},
	{"cancelled by user", "已由用户取消"},
	{"awaiting a session hook", "等待会话上报"},
}

// desktopWaits names what a "waiting for desktop: <code> <message>" reason is
// actually waiting on, so the user knows what to do.
var desktopWaits = map[string]string{
	"user_active":             "你正在使用键盘或鼠标；停手 3 秒后自动重试",
	"claude_in_use":           "你正在使用 Claude；为免切走你正在看的会话，Claude 空闲一段时间后再发送（可在「托管规则 › 发送方式」调整）",
	"screen_locked":           "屏幕已锁定；解锁后自动重试",
	"screen_inactive":         "屏幕未处于活动状态；唤醒后自动重试",
	"console_inactive":        "当前登录会话不在前台；切回后自动重试",
	"lock_state_unverified":   "无法确认锁屏状态；稍后自动重试",
	"idle_state_unverified":   "无法读取键鼠空闲时间；稍后自动重试",
	"accessibility_denied":    "缺少辅助功能权限；在概览中授权后自动重试",
	"bridge_unavailable":      "LimitBreak 主程序的发送通道未就绪；确认 LimitBreak.app 正在运行",
	"bridge_missing":          "LimitBreak 主程序的发送通道未就绪；确认 LimitBreak.app 正在运行",
	"bridge_not_found":        "LimitBreak 主程序的发送通道未就绪；确认 LimitBreak.app 正在运行",
	"terminal_session_open":   "终端会话仍开着但不在 tmux 中；关闭它后在后台续写",
	"terminal_not_foreground": "终端窗格里正在运行其他程序；回到 claude 后自动重试",
	"terminal_busy":           "终端会话正在运行；结束本轮后自动重试",
	"session_busy":            "会话正在处理上一轮；空闲后自动在后台直发",
}

func Explain(reason string) string {
	if reason == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(reason, "waiting for desktop: "); ok {
		code, _, _ := strings.Cut(rest, " ")
		if text, known := desktopWaits[code]; known {
			return "暂未发送：" + text
		}
	}
	if rest, ok := strings.CutPrefix(reason, "account concurrency limit: "); ok {
		var limit int
		var holders string
		if n, _ := fmt.Sscanf(rest, "%d per account, held by", &limit); n == 1 {
			if _, after, found := strings.Cut(rest, "held by "); found {
				holders = after
			}
		}
		if holders != "" {
			return fmt.Sprintf("账号并发已满（每账号 %d 个）：%s 正在用同一账号运行，它结束后立即继续；可在「托管规则」调高每账号并发，或给本会话指定其他账号", limit, holders)
		}
	}
	for _, e := range explanations {
		if strings.HasPrefix(reason, e.prefix) {
			return e.text
		}
	}
	return reason
}
