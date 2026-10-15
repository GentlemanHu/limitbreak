package domain

import (
	"strings"
	"testing"
)

func TestExplainNamesWhatADesktopWaitIsWaitingFor(t *testing.T) {
	cases := map[string]string{
		"waiting for desktop: user_active user typing":                                                      "暂未发送：你正在使用键盘或鼠标；停手 3 秒后自动重试",
		"waiting for desktop: screen_locked ":                                                               "暂未发送：屏幕已锁定；解锁后自动重试",
		"waiting for desktop: something_new detail":                                                         "等待桌面可用（锁屏、权限或目标窗口未就绪）",
		"未发送（input_unverified）：未找到可编辑的 Claude 输入框。":                                                         "未发送（input_unverified）：未找到可编辑的 Claude 输入框。",
		"confirmed by UserPromptSubmit":                                                                     "已由会话 Hook 确认发送成功",
		"waiting for desktop: claude_in_use Claude 正在使用":                                                    "暂未发送：你正在使用 Claude；为免切走你正在看的会话，Claude 空闲一段时间后再发送（可在「托管规则 › 发送方式」调整）",
		"submitted via direct channel; waiting for a fresh hook":                                            "已后台静默直发，会话已开始处理，等待会话确认",
		"submitted via direct channel after waking the session; waiting for a fresh hook":                   "已在后台唤醒会话并静默直发，会话已开始处理，等待会话确认",
		"submitted via the Claude window after the session process did not start; waiting for a fresh hook": "已通过 Claude 窗口发送（会话进程未能在后台启动，改用界面发送），等待会话确认",
	}
	for reason, want := range cases {
		if got := Explain(reason); got != want {
			t.Errorf("Explain(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestValidCatalogEntry(t *testing.T) {
	ok := CatalogEntry{HostSessionID: "local_0a1b2c3d-1111-4222-8333-444455556666", CLISessionID: "11111111-2222-4333-8444-555555555555", Cwd: "/Users/u/p", Title: "你是什么"}
	if !ValidCatalogEntry(ok) {
		t.Fatal("valid entry rejected")
	}
	for _, edit := range []func(*CatalogEntry){
		func(c *CatalogEntry) { c.HostSessionID = "local_x" },
		func(c *CatalogEntry) { c.CLISessionID = "../x" },
		func(c *CatalogEntry) { c.Cwd = "relative" },
		func(c *CatalogEntry) { c.Title = "a\nb" },
	} {
		bad := ok
		edit(&bad)
		if ValidCatalogEntry(bad) {
			t.Fatalf("invalid entry accepted: %+v", bad)
		}
	}
}

func TestExplainConcurrencyNamesHolder(t *testing.T) {
	got := Explain("account concurrency limit: 1 per account, held by 「Project A」")
	if !strings.Contains(got, "「Project A」") || !strings.Contains(got, "每账号 1 个") {
		t.Fatalf("holder not explained: %q", got)
	}
	if Explain("account concurrency limit") != "账号并发已满，排队中" {
		t.Fatal("bare reason must keep its text")
	}
}
