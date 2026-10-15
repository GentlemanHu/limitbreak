// Package bridge adapts the one-shot native macOS AX executable. It never uses
// CCD MCP, a shell, clipboard, coordinates, Apple Events or keyboard submission.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"quota-session-orchestrator/internal/domain"
)

const (
	maxRequest    = 131072
	maxOutput     = 65536
	probeTimeout  = 8 * time.Second
	resumeTimeout = 45 * time.Second
	wakeTimeout   = 15 * time.Second
)

// One process-wide, context-aware gate also serializes independent Client values.
// The Swift executable additionally takes a nonblocking per-user file lock.
var processGate = make(chan struct{}, 1)

// Client invokes only the explicit executable path supplied by the caller.
// Successful results are local AX submission evidence, NOT model completion.
type Client struct {
	path    string
	pathErr error
}

func New(path string) *Client {
	client := &Client{}
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		client.pathErr = errors.New("bridge executable path is required")
		return client
	}
	client.path, client.pathErr = filepath.Abs(path)
	return client
}

func (c *Client) Probe(ctx context.Context) domain.BridgeResult {
	result, _ := c.invoke(ctx, "--probe", nil, probeTimeout)
	return result
}

func (c *Client) Resume(ctx context.Context, binding domain.Binding, message string) (domain.BridgeResult, error) {
	// Local validation avoids starting a process for unsupported or unsafe requests.
	if binding.Kind != "code" {
		return closed("unsupported", "仅支持 Code；Chat/Cowork 不支持自动恢复。"), nil
	}
	if !binding.AutoResume || !binding.AcknowledgeSideEffects {
		return closed("consent_required", "绑定须启用自动恢复并明确确认副作用。"), nil
	}
	if binding.SessionID == "" || binding.ProjectMarker == "" ||
		(binding.TargetIdentifier == "" && binding.TargetTitle == "") {
		return closed("invalid_binding", "需要会话、精确目标和非空 ProjectMarker。"), nil
	}
	for _, value := range []string{binding.SessionID, binding.ProjectMarker, binding.TargetIdentifier, binding.TargetTitle} {
		if !utf8.ValidString(value) || len(value) > 4096 || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return closed("invalid_binding", "绑定标识格式不合法。"), nil
		}
	}
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" || len(message) > 65536 || strings.ContainsRune(message, '\x00') {
		return closed("invalid_message", "消息为空、过大或包含 NUL。"), nil
	}
	// Monitoring references are irrelevant to desktop identity and need not enter AX payloads.
	binding.MonitorResources = nil
	binding.ResumeResources = nil
	binding.MonitorMode = ""
	payload, err := json.Marshal(struct {
		Binding domain.Binding `json:"binding"`
		Message string         `json:"message"`
	}{binding, message})
	if err != nil || len(payload) > maxRequest {
		return closed("invalid_request", "请求不能编码或超过安全大小限制。"), errors.New("bridge request encoding failed")
	}
	return c.invoke(ctx, "--resume", payload, resumeTimeout)
}

func closed(code, message string) domain.BridgeResult {
	return domain.BridgeResult{Code: code, Message: message, ScreenLocked: true}
}

// capture drains stdout even after the limit, without retaining unbounded data.
type capture struct {
	data     []byte
	overflow bool
}

func (w *capture) Write(p []byte) (int, error) {
	n := len(p)
	available := maxOutput - len(w.data)
	if len(p) > available {
		w.overflow = true
		p = p[:available]
	}
	w.data = append(w.data, p...)
	return n, nil
}

func (c *Client) invoke(ctx context.Context, mode string, payload []byte, timeout time.Duration) (domain.BridgeResult, error) {
	if ctx == nil {
		return closed("invalid_context", "需要非空 context。"), errors.New("nil bridge context")
	}
	if runtime.GOOS != "darwin" {
		return closed("unsupported_platform", "原生 AX 桥接仅支持 macOS。"), nil
	}
	if c == nil || c.pathErr != nil || c.path == "" {
		return closed("bridge_unavailable", "未配置有效的桥接可执行路径。"), errors.New("invalid bridge executable path")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case processGate <- struct{}{}:
		defer func() { <-processGate }()
	case <-ctx.Done():
		return closed("cancelled", "桥接在启动前已取消。"), ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return closed("cancelled", "桥接在启动前已取消。"), err
	}
	command := exec.CommandContext(ctx, c.path, mode)
	command.Stdin = bytes.NewReader(payload)
	output := &capture{}
	command.Stdout = output
	// Do not retain child diagnostics, which might contain private request/AX text.
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	if err := command.Start(); err != nil {
		if ctx.Err() != nil {
			return closed("cancelled", "桥接在启动前已取消。"), ctx.Err()
		}
		return closed("bridge_unavailable", "无法启动指定的桥接程序。"), errors.New("bridge process start failed")
	}
	// CommandContext's default cancellation kills this child, not merely its wait.
	// Any loss of the reply AFTER Start is potentially after Send. Never retry it.
	waitErr := command.Wait()
	uncertain := func(code, message string, err error) (domain.BridgeResult, error) {
		result := closed(code, message)
		if mode == "--resume" {
			result.Submitted = true
			result.Code = "submitted_uncertain"
			result.Message = "恢复子进程已启动但未获得可靠结果；可能已提交，禁止自动重发。"
		}
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return uncertain("cancelled", "桥接已取消或超时。", err)
	}
	if waitErr != nil {
		return uncertain("bridge_failed", "桥接子进程退出异常。", errors.New("bridge process failed"))
	}
	if output.overflow {
		return uncertain("invalid_response", "桥接响应超过安全大小限制。", errors.New("bridge response exceeds limit"))
	}
	result, err := decodeResult(output.data, mode)
	if err != nil {
		return uncertain("invalid_response", "桥接响应缺失、格式错误或语义矛盾。", err)
	}
	return result, nil
}

// encoding/json normally accepts duplicate keys; forbid them in bridge replies.
func uniqueObject(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	token, err = decoder.Token()
	var trailing any
	return err == nil && token == json.Delim('}') && decoder.Decode(&trailing) == io.EOF
}

func decodeResult(data []byte, mode string) (domain.BridgeResult, error) {
	if !utf8.Valid(data) || !uniqueObject(data) {
		return domain.BridgeResult{}, errors.New("invalid or duplicate bridge JSON fields")
	}
	// Pointers detect omitted false-valued fields; a zero result is not a reply.
	var wire struct {
		OK                   *bool   `json:"ok"`
		Submitted            *bool   `json:"submitted"`
		Code                 *string `json:"code"`
		Message              *string `json:"message"`
		AccessibilityGranted *bool   `json:"accessibility_granted"`
		ScreenLocked         *bool   `json:"screen_locked"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return domain.BridgeResult{}, errors.New("invalid bridge JSON")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || wire.OK == nil || wire.Submitted == nil ||
		wire.Code == nil || wire.Message == nil || wire.AccessibilityGranted == nil || wire.ScreenLocked == nil {
		return domain.BridgeResult{}, errors.New("incomplete bridge JSON")
	}
	result := domain.BridgeResult{
		OK: *wire.OK, Submitted: *wire.Submitted, Code: *wire.Code, Message: *wire.Message,
		AccessibilityGranted: *wire.AccessibilityGranted, ScreenLocked: *wire.ScreenLocked,
	}
	if result.Code == "" || len(result.Code) > 128 || len(result.Message) > 4096 ||
		(result.OK && (result.ScreenLocked || (!result.AccessibilityGranted && mode != "--wake"))) ||
		(mode == "--probe" && (result.Submitted || (result.OK && result.Code != "ready"))) ||
		(mode == "--wake" && (result.Submitted || (result.OK && result.Code != "woken"))) ||
		(mode == "--resume" && result.OK && (!result.Submitted || result.Code != "submitted")) ||
		(result.Submitted && result.Code != "submitted" && result.Code != "submitted_uncertain") ||
		(result.Code == "submitted" && (!result.OK || !result.Submitted)) ||
		(result.Code == "submitted_uncertain" && (result.OK || !result.Submitted)) {
		return domain.BridgeResult{}, errors.New("inconsistent bridge result")
	}
	return result, nil
}
