package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"quota-session-orchestrator/internal/domain"
)

// Signal source types implemented in internal/signals. The list is closed: a
// new feed is added in code (see docs/EXTENDING.md), never loaded at runtime.
var SignalSourceTypes = []string{"codex-resets"}

// Signal source modes. "auto" polls only while at least one account of the
// source's provider exists.
const (
	SignalModeAuto = "auto"
	SignalModeOn   = "on"
	SignalModeOff  = "off"
)

// SignalSource configures one polled reset feed. URL and Provider default per
// type; URL must be HTTPS (literal loopback HTTP is allowed for tests).
type SignalSource struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Mode        string `json:"mode,omitempty"`
	PollSeconds int    `json:"poll_seconds,omitempty"`
}

func (s *SignalSource) UnmarshalJSON(data []byte) error {
	type plain SignalSource
	var value plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*s = SignalSource(value)
	return nil
}

// Signals configures reset signals. A nil Sources list (JSON "feeds") enables the default
// codex-resets feed in auto mode; an empty list disables polling. The webhook
// (POST /api/signals) always accepts the local token; it also accepts an
// HMAC-SHA256 signature when WebhookSecretFile names a 0600 file.
type Signals struct {
	Sources           []SignalSource `json:"feeds"`
	WebhookSecretFile string         `json:"webhook_secret_file,omitempty"`
}

// DefaultSignalSource is the source used when signals.feeds is absent.
func DefaultSignalSource() SignalSource {
	return SignalSource{ID: "codex-resets", Type: "codex-resets", Mode: SignalModeAuto}
}

// EffectiveSignalSources fills per-type defaults.
func (c Config) EffectiveSignalSources() []SignalSource {
	sources := c.Signals.Sources
	if sources == nil {
		sources = []SignalSource{DefaultSignalSource()}
	}
	out := make([]SignalSource, 0, len(sources))
	for _, s := range sources {
		if s.Type == "codex-resets" {
			if s.URL == "" {
				s.URL = "https://codex-resets.com"
			}
			if s.Provider == "" {
				s.Provider = "codex"
			}
		}
		if s.Mode == "" {
			s.Mode = SignalModeAuto
		}
		if s.PollSeconds == 0 {
			s.PollSeconds = 60
		}
		out = append(out, s)
	}
	return out
}

func (c Config) validateSignals() error {
	seen := map[string]bool{}
	sources := c.EffectiveSignalSources()
	if len(sources) > 8 {
		return errors.New("最多配置 8 个重置信号来源")
	}
	for _, s := range sources {
		if !sourceIDPattern.MatchString(s.ID) || s.ID == "webhook" {
			return fmt.Errorf("重置信号来源 ID 无效：%q", s.ID)
		}
		if seen[s.ID] {
			return fmt.Errorf("重置信号来源 ID 重复：%s", s.ID)
		}
		seen[s.ID] = true
		known := false
		for _, t := range SignalSourceTypes {
			known = known || t == s.Type
		}
		if !known {
			return fmt.Errorf("不支持的重置信号来源类型：%q", s.Type)
		}
		if err := ValidateSourceURL(s.URL); err != nil {
			return fmt.Errorf("重置信号来源 %s：%w", s.ID, err)
		}
		if !domain.ValidSignalProvider(s.Provider) {
			return fmt.Errorf("重置信号来源 %s 的 provider 无效", s.ID)
		}
		if s.Mode != SignalModeAuto && s.Mode != SignalModeOn && s.Mode != SignalModeOff {
			return fmt.Errorf("重置信号来源 %s 的 mode 只能为 auto、on 或 off", s.ID)
		}
		if s.PollSeconds < 30 || s.PollSeconds > 86400 {
			return fmt.Errorf("重置信号来源 %s 的轮询间隔必须为 30–86400 秒", s.ID)
		}
	}
	return nil
}
