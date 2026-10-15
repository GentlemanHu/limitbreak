package domain

import (
	"errors"
	"strings"
)

// SessionSettings is a partial update; nil fields are left unchanged.
type SessionSettings struct {
	HostMode        *string   `json:"host_mode,omitempty"`
	ContinueMessage *string   `json:"continue_message,omitempty"`
	PoolOverride    *[]string `json:"pool_override,omitempty"`
	TargetOverride  *string   `json:"target_override,omitempty"`
	Priority        *int      `json:"priority,omitempty"`
}

// ValidTargetOverride accepts "" (follow origin) or "desktop".
func ValidTargetOverride(v string) bool {
	return v == "" || v == "desktop"
}

func ValidateSessionSettings(s SessionSettings) error {
	if s.HostMode != nil && !ValidHostMode(*s.HostMode, true) {
		return errors.New("托管模式必须为 hosted、monitor、off 或留空跟随全局")
	}
	if s.ContinueMessage != nil && *s.ContinueMessage != "" && !ValidContinueMessage(*s.ContinueMessage) {
		return errors.New("继续文案须为单行、非空、≤4096 字节；留空跟随全局")
	}
	if s.TargetOverride != nil && !ValidTargetOverride(*s.TargetOverride) {
		return errors.New("发送目标只能为空（跟随来源）或 desktop")
	}
	if s.Priority != nil && (*s.Priority < -100 || *s.Priority > 100) {
		return errors.New("优先级范围为 -100 到 100")
	}
	if s.PoolOverride != nil {
		if len(*s.PoolOverride) > 256 {
			return errors.New("账号池最多 256 个账号")
		}
		seen := map[string]bool{}
		for _, id := range *s.PoolOverride {
			if strings.TrimSpace(id) == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
				return errors.New("账号池 ID 无效或重复")
			}
			seen[id] = true
		}
	}
	return nil
}

// Apply copies the non-nil settings onto the session.
func (s SessionSettings) Apply(session *Session) {
	if s.HostMode != nil {
		session.HostMode = *s.HostMode
	}
	if s.ContinueMessage != nil {
		session.ContinueMessage = *s.ContinueMessage
	}
	if s.PoolOverride != nil {
		session.PoolOverride = append([]string(nil), (*s.PoolOverride)...)
	}
	if s.TargetOverride != nil {
		session.TargetOverride = *s.TargetOverride
	}
	if s.Priority != nil {
		session.Priority = *s.Priority
	}
}

// UserError is a locally authored, display-safe refusal. Callers may show its
// text; any other error is internal and is reported generically.
type UserError string

func (e UserError) Error() string { return string(e) }

// ErrSessionNotFound is returned when an action names an unknown session.
const ErrSessionNotFound = UserError("会话不存在")
