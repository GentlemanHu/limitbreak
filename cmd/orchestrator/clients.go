package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

// loadWebhookSecret reads the optional HMAC secret for POST /api/signals. A
// missing file disables signed webhooks; any other problem is a startup error.
func loadWebhookSecret(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > 4096 {
		return nil, errors.New("Webhook 密钥必须为权限 0600 的普通文件，且不超过 4 KiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret := strings.TrimSpace(string(b))
	if len(secret) < 32 {
		return nil, errors.New("Webhook 密钥至少 32 字符")
	}
	return []byte(secret), nil
}

// sendSignal posts one reset signal with the local token.
func sendSignal(path string, args []string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("signal", flag.ContinueOnError)
	provider := f.String("provider", "", "额度提供方，例如 codex、claude")
	id := f.String("id", "", "重置事件的唯一 ID（重复 ID 会被忽略）")
	source := f.String("source", "cli", "信号来源名称")
	kind := f.String("kind", domain.SignalResetExecuted, "reset_executed 或 reset_scheduled")
	text := f.String("text", "", "可选说明")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() > 0 {
		return errors.New("signal 不接受多余参数")
	}
	b, err := json.Marshal(map[string]any{"source": *source, "provider": *provider, "id": *id, "kind": *kind, "at": time.Now().UTC(), "text": *text})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := call(ctx, c, "POST", "/api/signals", b)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(out)))
	return nil
}

// clientEventTypes maps the generic client vocabulary onto lifecycle events.
var clientEventTypes = map[string]struct{ hook, errorCode string }{
	"limit_hit":     {"StopFailure", "rate_limit"},
	"turn_failed":   {"StopFailure", "client_error"},
	"turn_started":  {"UserPromptSubmit", ""},
	"turn_finished": {"Stop", ""},
	"session_start": {"SessionStart", ""},
	"session_end":   {"SessionEnd", ""},
}

// forwardEvent reports one lifecycle event of a configured generic client.
func forwardEvent(path string, args []string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	e, err := clientEvent(c, args, time.Now())
	if err != nil {
		return err
	}
	return spoolAndDeliver(path, c, e)
}

func clientEvent(c config.Config, args []string, now time.Time) (domain.HookEvent, error) {
	f := flag.NewFlagSet("event", flag.ContinueOnError)
	clientID := f.String("client", "", "clients 中配置的客户端 ID")
	session := f.String("session", "", "客户端自己的会话 ID")
	kind := f.String("type", "", "事件类型")
	cwd := f.String("cwd", "", "会话工作目录（绝对路径）")
	model := f.String("model", "", "模型名称")
	title := f.String("title", "", "会话标题")
	eventID := f.String("event-id", "", "幂等事件 ID；省略时随机生成")
	var e domain.HookEvent
	if err := f.Parse(args); err != nil {
		return e, err
	}
	if f.NArg() > 0 {
		return e, errors.New("event 不接受多余参数")
	}
	client, ok := c.Client(*clientID)
	if !ok {
		return e, fmt.Errorf("客户端 %q 未在配置的 clients 中", *clientID)
	}
	if !domain.ValidClientSessionID(*session) {
		return e, errors.New("会话 ID 无效（字母数字开头，可含 . _ : -，最长 128）")
	}
	mapped, ok := clientEventTypes[*kind]
	if !ok {
		return e, errors.New("事件类型无效：limit_hit、turn_started、turn_finished、turn_failed、session_start、session_end")
	}
	if *cwd != "" && !domain.ValidAbsolutePath(*cwd) {
		return e, errors.New("--cwd 必须为绝对路径")
	}
	if len(*model) > 128 || len(*title) > 500 || strings.ContainsAny(*model+*title, "\x00\r\n") {
		return e, errors.New("模型或标题无效")
	}
	if *eventID == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return e, err
		}
		*eventID = hex.EncodeToString(b)
	} else if len(*eventID) > 200 || strings.ContainsAny(*eventID, "\x00\r\n") {
		return e, errors.New("--event-id 无效")
	}
	e = domain.HookEvent{
		// Namespaced so a client's IDs never deduplicate against Code hooks.
		EventID: "client:" + client.ID + ":" + *eventID, SessionID: *session, HookEventName: mapped.hook,
		Error: mapped.errorCode, Cwd: *cwd, Model: *model, SessionTitle: *title, Provider: client.Provider,
		Kind: "code", ObservedAt: now.UTC(),
		Origin: &domain.SessionOrigin{App: domain.AppClient, Client: client.ID, ObservedAt: now.UTC()},
	}
	return e, nil
}
