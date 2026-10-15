package config

import (
	"errors"
	"path/filepath"
	"strings"

	"quota-session-orchestrator/internal/domain"
)

// EffectivePolicy supplies only the optional v3.1 defaults. Persisted, nonzero
// values (including the user's send interval) are never overwritten.
func EffectivePolicy(p domain.Policy) domain.Policy {
	if p.EarlyProbeSeconds == 0 {
		p.EarlyProbeSeconds = 120
	}
	if p.NearResetProbeSeconds == 0 {
		p.NearResetProbeSeconds = 30
	}
	if p.ResetLookaheadSeconds == 0 {
		p.ResetLookaheadSeconds = 300
	}
	if p.UserIdleSeconds == 0 {
		p.UserIdleSeconds = 3
	}
	if p.RunningStaleSeconds == 0 {
		p.RunningStaleSeconds = 900
	}
	if p.QueueAgingSeconds == 0 {
		p.QueueAgingSeconds = 60
	}
	if p.MaxAutoResumesPerHour == 0 {
		p.MaxAutoResumesPerHour = 3
	}
	if p.NativeGraceSeconds == 0 {
		p.NativeGraceSeconds = 90
	}
	if p.ClaudeQuietSeconds == 0 {
		p.ClaudeQuietSeconds = 120
	}
	return p
}

func validateOptionalPolicy(p domain.Policy) error {
	for _, field := range []struct{ value, low, high int }{
		{p.EarlyProbeSeconds, 30, 3600}, {p.NearResetProbeSeconds, 30, 300},
		{p.ResetLookaheadSeconds, 30, 86400}, {p.UserIdleSeconds, 1, 3600},
		{p.RunningStaleSeconds, 60, 86400}, {p.QueueAgingSeconds, 1, 86400},
		{p.MaxAutoResumesPerHour, 1, 100}, {p.NativeGraceSeconds, 1, 3600},
		{p.ClaudeQuietSeconds, 1, 3600},
	} {
		if field.value != 0 && (field.value < field.low || field.value > field.high) {
			return errors.New("可选监控调度策略超出合法范围；0 使用兼容默认值")
		}
	}
	if !domain.ValidHostMode(p.DefaultHostMode, true) {
		return errors.New("默认托管模式无效")
	}
	if len(p.ModelAliases) > 64 {
		return errors.New("模型映射最多 64 条")
	}
	for from, to := range p.ModelAliases {
		if domain.NormalizeModel(from) != from || from == "" || to == "" || len(from) > 128 || len(to) > 128 || strings.ContainsAny(from+to, "\x00\r\n") {
			return errors.New("模型映射无效：键须为规范化小写模型名")
		}
	}
	return nil
}

func validateDesktopIPC(c Config) error {
	if c.DesktopSocket == "" && c.DesktopTokenFile == "" {
		return nil
	}
	if c.DesktopSocket == "" || c.DesktopTokenFile == "" {
		return errors.New("desktop_socket 与 desktop_token_file 必须成对配置")
	}
	for _, path := range []string{c.DesktopSocket, c.DesktopTokenFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("Desktop IPC 路径必须为规范绝对路径")
		}
	}
	// Darwin sockaddr_un has a 104-byte path buffer including its terminator.
	if len(c.DesktopSocket) > 100 || len(c.DesktopTokenFile) > 4096 || c.DesktopSocket == c.DesktopTokenFile {
		return errors.New("Desktop IPC 路径重复或长度无效")
	}
	dir := filepath.Dir(c.DesktopSocket)
	if dir != filepath.Dir(c.DesktopTokenFile) || dir == string(filepath.Separator) || dir == "/tmp" || dir == "/private/tmp" {
		return errors.New("Desktop IPC 必须位于同一专用私有目录")
	}
	// Ownership, 0600 and symlink checks are performed by the IPC client on use.
	return nil
}
