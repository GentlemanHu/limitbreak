package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"

	"quota-session-orchestrator/internal/domain"
)

// These are pure protocol/validation tests; they never invoke Claude or AX.
func TestResultProtocol(t *testing.T) {
	ready := domain.BridgeResult{OK: true, Code: "ready", AccessibilityGranted: true}
	sent := domain.BridgeResult{OK: true, Submitted: true, Code: "submitted", AccessibilityGranted: true}
	uncertain := domain.BridgeResult{Submitted: true, Code: "submitted_uncertain", ScreenLocked: true}
	for _, test := range []struct {
		mode   string
		result domain.BridgeResult
	}{
		{"--probe", ready}, {"--resume", sent}, {"--resume", uncertain},
		{"--resume", closed("identity_unverified", "无法确认身份。")},
	} {
		data, err := json.Marshal(test.result)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeResult(data, test.mode)
		if err != nil || got != test.result {
			t.Fatalf("protocol %s: err=%v", test.mode, err)
		}
	}
}

func TestRejectsUnsafeReplies(t *testing.T) {
	valid := `{"ok":true,"submitted":true,"code":"submitted","message":"","accessibility_granted":true,"screen_locked":false}`
	cases := []string{
		`{}`, `null`, `[]`, valid + `{}`, valid + "garbage",
		strings.Replace(valid, `"ok":true`, `"ok":null`, 1),
		strings.Replace(valid, `"ok":true`, `"ok":false,"ok":true`, 1),
		strings.Replace(valid, `"submitted":true`, `"submitted":false`, 1),
		strings.Replace(valid, `"screen_locked":false`, `"screen_locked":true`, 1),
		strings.Replace(valid, `"accessibility_granted":true`, `"accessibility_granted":false`, 1),
		strings.Replace(valid, `"message":""`, `"message":"","transcript":"private"`, 1),
		strings.Replace(valid, `"code":"submitted"`, `"code":"submitted_uncertain"`, 1),
	}
	for index, data := range cases {
		if _, err := decodeResult([]byte(data), "--resume"); err == nil {
			t.Fatalf("accepted unsafe reply %d", index)
		}
	}
	if _, err := decodeResult([]byte(valid), "--probe"); err == nil {
		t.Fatal("probe accepted submitted result")
	}
}

func TestValidationDoesNotStartProcess(t *testing.T) {
	binding := domain.Binding{
		SessionID: "enrolled", Kind: "code", ProjectMarker: "/enrolled/project",
		TargetTitle: "enrolled session", AutoResume: true, AcknowledgeSideEffects: true,
	}
	client := New("/bridge-validation-must-not-execute")
	cases := []struct {
		change  func(*domain.Binding)
		message string
		code    string
	}{
		{func(b *domain.Binding) { b.Kind = "chat" }, "continue", "unsupported"},
		{func(b *domain.Binding) { b.Kind = "cowork" }, "continue", "unsupported"},
		{func(b *domain.Binding) { b.AutoResume = false }, "continue", "consent_required"},
		{func(b *domain.Binding) { b.AcknowledgeSideEffects = false }, "continue", "consent_required"},
		{func(b *domain.Binding) { b.ProjectMarker = "" }, "continue", "invalid_binding"},
		{func(b *domain.Binding) { b.TargetTitle = "" }, "continue", "invalid_binding"},
		{func(b *domain.Binding) { b.TargetTitle = " padded " }, "continue", "invalid_binding"},
		{func(b *domain.Binding) { b.TargetTitle = string([]byte{0xff}) }, "continue", "invalid_binding"},
		{func(b *domain.Binding) {}, " \n", "invalid_message"},
		{func(b *domain.Binding) {}, "text\x00", "invalid_message"},
		{func(b *domain.Binding) {}, string([]byte{0xff}), "invalid_message"},
		{func(b *domain.Binding) {}, strings.Repeat("a", 65537), "invalid_message"},
	}
	for index, test := range cases {
		value := binding
		test.change(&value)
		result, err := client.Resume(context.Background(), value, test.message)
		if err != nil || result.Code != test.code || result.OK || result.Submitted {
			t.Fatalf("validation %d: result=%+v err=%v", index, result, err)
		}
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS adapter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := New("/bridge-cancel-must-not-execute")
	result, err := client.invoke(ctx, "--resume", nil, resumeTimeout)
	if result.Submitted || result.OK || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-start cancellation: result=%+v err=%v", result, err)
	}
}

func TestBoundedCapture(t *testing.T) {
	writer := &capture{}
	large := make([]byte, maxOutput+1)
	if n, err := writer.Write(large); n != len(large) || err != nil {
		t.Fatal("capture did not drain output")
	}
	if !writer.overflow || len(writer.data) != maxOutput {
		t.Fatal("capture not bounded")
	}
	if n, err := writer.Write(large); n != len(large) || err != nil || len(writer.data) != maxOutput {
		t.Fatal("overflow grew retained output")
	}
}
