package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"quota-session-orchestrator/internal/domain"
)

// Client placeholders. Each must be a whole argument; they are substituted
// without a shell, so a value can never become a second argument or a
// command. {message} is required exactly once.
const (
	PlaceholderSessionID = "{session_id}"
	PlaceholderCwd       = "{cwd}"
	PlaceholderMessage   = "{message}"
)

var envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// Client is a generic agent client (Codex CLI, another coding agent or a
// script) that reports its sessions with `orchestrator event --client <id>`
// and is continued by running Command. Command[0] must be an absolute path to
// an executable. Env lists environment variable names copied from the daemon's
// environment; besides them only HOME, USER, LOGNAME, PATH, LANG, LC_* and
// TMPDIR are inherited.
//
// The command must continue exactly the given session. Starting it is not
// proof the turn ran, and its exit status is not consulted: the job stays
// unconfirmed until the client reports turn_started or turn_finished, and is
// never resent automatically. It is killed after TimeoutSeconds.
type Client struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	Provider       string   `json:"provider,omitempty"`
	Command        []string `json:"command"`
	Env            []string `json:"env,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

func (c *Client) UnmarshalJSON(data []byte) error {
	type plain Client
	var value plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*c = Client(value)
	return nil
}

// EffectiveClients fills defaults: the name falls back to the ID and the
// timeout to 120 seconds.
func (c Config) EffectiveClients() []Client {
	out := make([]Client, 0, len(c.Clients))
	for _, client := range c.Clients {
		if client.Name == "" {
			client.Name = client.ID
		}
		if client.TimeoutSeconds == 0 {
			client.TimeoutSeconds = 120
		}
		out = append(out, client)
	}
	return out
}

// Client returns the configured client id.
func (c Config) Client(id string) (Client, bool) {
	for _, client := range c.EffectiveClients() {
		if client.ID == id {
			return client, true
		}
	}
	return Client{}, false
}

func (c Config) validateClients() error {
	clients := c.EffectiveClients()
	if len(clients) > 16 {
		return errors.New("最多配置 16 个客户端")
	}
	seen := map[string]bool{}
	for _, client := range clients {
		if !domain.ValidClientID(client.ID) {
			return fmt.Errorf("客户端 ID 无效：%q（小写字母、数字及 . _ -）", client.ID)
		}
		if seen[client.ID] {
			return fmt.Errorf("客户端 ID 重复：%s", client.ID)
		}
		seen[client.ID] = true
		if len(client.Name) > 64 || strings.ContainsAny(client.Name, "\x00\r\n") {
			return fmt.Errorf("客户端 %s 的名称无效", client.ID)
		}
		if client.Provider != "" && !domain.ValidSignalProvider(client.Provider) {
			return fmt.Errorf("客户端 %s 的 provider 无效", client.ID)
		}
		if err := validateCommand(client.Command); err != nil {
			return fmt.Errorf("客户端 %s：%w", client.ID, err)
		}
		if len(client.Env) > 16 {
			return fmt.Errorf("客户端 %s 最多透传 16 个环境变量", client.ID)
		}
		for _, name := range client.Env {
			if !envNamePattern.MatchString(name) {
				return fmt.Errorf("客户端 %s 的环境变量名无效：%q", client.ID, name)
			}
		}
		if client.TimeoutSeconds < 5 || client.TimeoutSeconds > 3600 {
			return fmt.Errorf("客户端 %s 的超时必须为 5–3600 秒", client.ID)
		}
	}
	return nil
}

func validateCommand(argv []string) error {
	if len(argv) == 0 || len(argv) > 32 {
		return errors.New("command 必须为 1–32 个参数")
	}
	if !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] {
		return errors.New("command[0] 必须为可执行文件的绝对路径（不经过 shell）")
	}
	messages := 0
	for _, arg := range argv {
		if len(arg) > 1024 || strings.ContainsRune(arg, 0) {
			return errors.New("command 参数过长或包含 NUL")
		}
		if arg == PlaceholderMessage {
			messages++
		}
		if strings.Contains(arg, "{") && arg != PlaceholderSessionID && arg != PlaceholderCwd && arg != PlaceholderMessage {
			return fmt.Errorf("占位符必须单独作为一个参数：%q（可用 {session_id} {cwd} {message}）", arg)
		}
	}
	if messages != 1 {
		return errors.New("command 必须恰好包含一个 {message} 参数")
	}
	return nil
}
