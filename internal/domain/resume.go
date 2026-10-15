package domain

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Empty ownership is deliberately unresolved for upgraded bindings.
func ValidateResumeFields(b Binding) error {
	switch b.ResumeOwner {
	case "", "unknown", "guard", "native", "manual", "auto":
	default:
		return errors.New("resume_owner must be guard, native, manual, auto or unresolved")
	}
	if b.Priority < -100 || b.Priority > 100 {
		return errors.New("binding priority must be between -100 and 100")
	}
	if b.AutoResume && b.ResumeOwner == "guard" && !b.NativeAutoResumeDisabledAcknowledged {
		return errors.New("guard ownership requires explicit confirmation that native auto-resume is disabled")
	}
	if b.ContinueMessage != "" && (!utf8.ValidString(b.ContinueMessage) || strings.TrimSpace(b.ContinueMessage) == "" || len(b.ContinueMessage) > 4096 || strings.ContainsRune(b.ContinueMessage, '\x00')) {
		return errors.New("per-session continue_message must be valid nonempty text of at most 4096 bytes")
	}
	for _, identifier := range []string{b.TargetWindowIdentifier, b.TargetPaneIdentifier} {
		if !utf8.ValidString(identifier) || len(identifier) > 4096 || strings.TrimSpace(identifier) != identifier || strings.IndexFunc(identifier, unicode.IsControl) >= 0 {
			return errors.New("desktop window or pane identifier is invalid")
		}
	}
	return nil
}

// ValidContinueMessage applies to every executor: one line, no NUL, ≤4096 bytes.
func ValidContinueMessage(m string) bool {
	return utf8.ValidString(m) && strings.TrimSpace(m) != "" && len(m) <= 4096 && !strings.ContainsAny(m, "\x00\r\n")
}

func EffectiveContinueMessage(b Binding, p Policy) string {
	if b.ContinueMessage != "" {
		return b.ContinueMessage
	}
	return p.ContinueMessage
}
