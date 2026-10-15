package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"quota-session-orchestrator/internal/domain"
)

type QuotaQuery struct {
	Provider      string            `json:"provider"`
	PluginID      string            `json:"plugin_id,omitempty"`
	URL           string            `json:"url,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	RateLimitPath string            `json:"rate_limit_path,omitempty"`
	Models        []string          `json:"models"`
}

type Config struct {
	Listen             string        `json:"listen"`
	ProxyURL           string        `json:"proxy_url"`
	ManagementKeyEnv   string        `json:"management_key_env"`
	ManagementKeyFile  string        `json:"management_key_file"`
	ManagementPrefix   string        `json:"management_prefix"`
	Token              string        `json:"token"`
	Database           string        `json:"database"`
	BridgePath         string        `json:"bridge_path"`
	DesktopSocket      string        `json:"desktop_socket,omitempty"`
	DesktopTokenFile   string        `json:"desktop_token_file,omitempty"`
	OnlyEmail          string        `json:"only_email"`
	PreferPlus         bool          `json:"prefer_plus"`
	PollSeconds        int           `json:"poll_seconds"`
	HTTPTimeoutSeconds int           `json:"http_timeout_seconds"`
	Queries            []QuotaQuery  `json:"queries"`
	Sources            []Source      `json:"sources,omitempty"`
	Signals            Signals       `json:"signals"`
	Clients            []Client      `json:"clients,omitempty"`
	Policy             domain.Policy `json:"policy"`
}

func Default(base string) Config {
	return Config{
		Listen: "127.0.0.1:8318", ProxyURL: "http://127.0.0.1:8317",
		ManagementKeyEnv: "CPA_MANAGEMENT_KEY", ManagementKeyFile: filepath.Join(base, "management-key"), ManagementPrefix: "auto",
		Database:   filepath.Join(base, "state.sqlite"),
		Signals:    Signals{WebhookSecretFile: filepath.Join(base, "webhook-secret")},
		BridgePath: filepath.Join(base, "bin", "desktop-bridge"), PreferPlus: true,
		PollSeconds: 180, HTTPTimeoutSeconds: 25,
		Policy: domain.Policy{Paused: true, MaxRunning: 2, MaxPerAccount: 1, SafetySeconds: 300,
			FreshnessSeconds: 240, VerifySeconds: 45, LongProbeSeconds: 3600,
			UnknownProbeSeconds: 900, MinSendIntervalSeconds: 30,
			EarlyProbeSeconds: 120, NearResetProbeSeconds: 30, ResetLookaheadSeconds: 300,
			UserIdleSeconds: 3, RunningStaleSeconds: 900, QueueAgingSeconds: 60, MaxAutoResumesPerHour: 3,
			ContinueMessage: "继续，从上次中断处接着做，不要重复已经完成的内容。先检查上一回合的实际完成情况，再继续下一步；对可能产生外部副作用的操作先核验是否已经执行。"},
	}
}

func Load(path string) (Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Config{}, err
	}
	path = abs
	c := Default(filepath.Dir(path))
	st, err := os.Lstat(path)
	if err != nil {
		return c, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 1<<20 {
		return c, errors.New("配置必须为权限 0600 的普通文件，且不超过 1 MiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	for _, p := range []*string{&c.Database, &c.BridgePath, &c.ManagementKeyFile, &c.Signals.WebhookSecretFile} {
		if *p == "" {
			continue
		}
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("listen 必须为显式 loopback IP:port")
	}
	u, err := url.Parse(c.ProxyURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("proxy_url 无效")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("非本地代理必须使用 HTTPS")
		}
	}
	if c.ManagementPrefix != "auto" && c.ManagementPrefix != "/v0/management" && c.ManagementPrefix != "/v8/management" {
		return errors.New("management_prefix 仅支持 auto、/v0/management、/v8/management")
	}
	if len(c.Token) < 32 {
		return errors.New("本地令牌至少 32 字符，请使用 init 生成")
	}
	if c.ManagementKeyEnv == "" || ValidateCredentialEnv(c.ManagementKeyEnv) != nil {
		return errors.New("management_key_env 无效")
	}
	if c.PollSeconds < 30 || c.HTTPTimeoutSeconds < 1 || c.HTTPTimeoutSeconds > 120 {
		return errors.New("轮询至少 30 秒，HTTP 超时为 1–120 秒")
	}
	if err := validateDesktopIPC(c); err != nil {
		return err
	}
	if err := ValidatePolicy(c.Policy); err != nil {
		return err
	}
	for _, q := range c.Queries {
		if q.Provider == "" {
			return errors.New("额度查询必须指定 provider")
		}
		if q.PluginID != "" && strings.ContainsAny(q.PluginID, "/?#") {
			return errors.New("plugin_id 只能是单一路径段")
		}
		if q.URL != "" {
			u, err := url.Parse(q.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return fmt.Errorf("%s 额度 URL 必须为 HTTPS", q.Provider)
			}
		}
	}
	if err := c.validateSignals(); err != nil {
		return err
	}
	if err := c.validateClients(); err != nil {
		return err
	}
	return c.validateSources()
}

func ValidatePolicy(p domain.Policy) error {
	if p.MaxRunning < 1 || p.MaxRunning > 16 || p.MaxPerAccount < 1 || p.MaxPerAccount > p.MaxRunning {
		return errors.New("并发限制无效")
	}
	if p.SafetySeconds < 0 || p.SafetySeconds > 3600 || p.FreshnessSeconds < 1 || p.FreshnessSeconds > 900 {
		return errors.New("安全余量或证据有效期无效")
	}
	if p.VerifySeconds < 5 || p.VerifySeconds > 300 || p.LongProbeSeconds < 60 || p.LongProbeSeconds > 86400 || p.UnknownProbeSeconds < 60 || p.UnknownProbeSeconds > 86400 || p.MinSendIntervalSeconds < 30 || p.MinSendIntervalSeconds > 86400 {
		return errors.New("调度间隔无效")
	}
	if err := validateOptionalPolicy(p); err != nil {
		return err
	}
	if !domain.ValidDirectDelivery(p.DirectDelivery) {
		return errors.New("后台直发设置只能为 auto 或 off")
	}
	if !domain.ValidResetFallback(p.ResetFallback) {
		return errors.New("重置后尝试继续的设置只能为 auto 或 off")
	}
	if !domain.ValidSwitch(p.RestoreFocus) || !domain.ValidSwitch(p.ClaudeQuiet) {
		return errors.New("恢复焦点与等待 Claude 空闲的设置只能为 auto 或 off")
	}
	if strings.TrimSpace(p.ContinueMessage) == "" || len(p.ContinueMessage) > 4096 || strings.ContainsAny(p.ContinueMessage, "\x00\r\n") {
		return errors.New("继续指令长度无效")
	}
	return nil
}
